import asyncio
import imaplib
import email
import os
import configparser
import telegram
from telegram import Bot
from telegram.error import TelegramError, NetworkError, RetryAfter
from google.cloud import speech_v1p1beta1 as speech
from google.oauth2 import service_account
from pydub import AudioSegment
from pathlib import Path
import logging
import time
import nest_asyncio
import random

# Apply the nest_asyncio patch
nest_asyncio.apply()

# Set up logging directory and file
log_dir = Path(__file__).resolve().parent / 'logs'
log_dir.mkdir(exist_ok=True)
log_file = log_dir / 'voicemail_notifier.log'

# Set up logging
logging.basicConfig(
    level=logging.DEBUG,
    format='%(asctime)s - %(levelname)s - %(message)s',
    handlers=[
        logging.StreamHandler(),
        logging.FileHandler(log_file)
    ]
)

config_dir = Path(__file__).resolve().parent / 'config'
config_file = config_dir / 'config.ini'
googlekey = config_dir / 'googlekey.json'
config = configparser.ConfigParser()
config.read(config_file)

IMAP_SERVER = config['secrets']['IMAPSERVER']
IMAP_PORT = int(config['secrets']['IMAPPORT'])
EMAIL = config['secrets']['EMAIL']
PASSWORD = config['secrets']['PASSWORD']
TELEGRAM_TOKEN = config['secrets']['TELEGRAMTOKEN']
CHAT_ID = config['secrets']['CHATID']

# Google API
credentials = service_account.Credentials.from_service_account_file(googlekey)
client = speech.SpeechClient(credentials=credentials)

def split_text(text, max_length):
    parts = []
    while text:
        part = text[:max_length]
        parts.append(part)
        text = text[max_length:]
    return parts

async def convert_speech_to_text_inline(audio_content):
    try:
        language_code = "nl-NL"
        sample_rate_hertz = 8000

        audio = speech.RecognitionAudio(content=audio_content)
        config = speech.RecognitionConfig(
            encoding=speech.RecognitionConfig.AudioEncoding.LINEAR16,
            sample_rate_hertz=sample_rate_hertz,
            language_code=language_code,
        )

        operation = client.long_running_recognize(config=config, audio=audio)
        logging.info("Waiting for operation to complete...")
        response = operation.result(timeout=300)

        transcript = ""
        for result in response.results:
            transcript += result.alternatives[0].transcript

        return transcript
    except Exception as e:
        logging.error(f"Error in convert_speech_to_text_inline: {e}")
        return ""

def split_audio(audio_path, segment_duration_ms):
    try:
        audio = AudioSegment.from_file(audio_path)
        segments = []
        
        start = 0
        while start < len(audio):
            end = start + segment_duration_ms
            segment = audio[start:end]
            segments.append(segment)
            start = end
        
        return segments
    except Exception as e:
        logging.error(f"Error in split_audio: {e}")
        return []

async def process_and_combine_segments(segments):
    try:
        combined_transcription = ""

        for segment in segments:
            text = await convert_speech_to_text_inline(segment.raw_data)
            combined_transcription += text + " "

        return combined_transcription.strip()
    except Exception as e:
        logging.error(f"Error in process_and_combine_segments: {e}")
        return ""

async def send_telegram_message_async(texts, audio_path):
    """
    Send Telegram messages with an audio file. If a single message exceeds the maximum caption length,
    split it into multiple parts and send each part as a separate message with the audio attached.
    """
    try:
        bot = Bot(token=TELEGRAM_TOKEN)
        for text in texts:
            # Split long text into multiple parts
            text_parts = split_text(text, 1024 - 20)  # Leave room for "Part x/y: " prefix
            for i, part in enumerate(text_parts, start=1):
                caption = f"Part {i}/{len(text_parts)}: " + part
                retry = True
                attempt = 0
                while retry and attempt < 5:
                    try:
                        # Re-open the audio file for each message part
                        with open(audio_path, 'rb') as audio_file:
                            await bot.send_voice(chat_id=CHAT_ID, voice=audio_file, caption=caption, parse_mode="Markdown")
                        retry = False
                    except NetworkError as e:
                        attempt += 1
                        wait_time = min(60, 2 ** attempt + random.random() * attempt)
                        logging.error(f"NetworkError: {e}, retrying in {wait_time} seconds... (Attempt {attempt}/5)")
                        await asyncio.sleep(wait_time)
                    except RetryAfter as e:
                        logging.error(f"Rate limited by Telegram, retrying after {e.retry_after} seconds...")
                        await asyncio.sleep(e.retry_after)
                    except TelegramError as e:
                        logging.error(f"TelegramError: {e}")
                        retry = False
                    except Exception as e:
                        logging.error(f"Error in send_telegram_message_async: {e}")
                        retry = False
    except Exception as e:
        logging.error(f"Error in send_telegram_message_async: {e}")

async def main_async():
    while True:
        try:
            mail = imaplib.IMAP4_SSL(IMAP_SERVER, IMAP_PORT)
            mail.login(EMAIL, PASSWORD)
            mail.select("inbox")
            logging.info("Connected to email server and selected inbox.")

            result, data = mail.search(None, 'UNSEEN SUBJECT "PBX"')
            email_ids = data[0].split()

            if email_ids:
                for email_id in email_ids:
                    try:
                        result, email_data = mail.fetch(email_id, "(RFC822)")
                        raw_email = email_data[0][1]
                        msg = email.message_from_bytes(raw_email)

                        subject = msg["subject"]
                        email_text = ""
                        combined_text = ""

                        for part in msg.walk():
                            if part.get_content_type() == "text/plain":
                                email_text = part.get_payload(decode=True).decode("utf-8")
                            elif part.get_content_type().startswith("audio/"):
                                audio_content = part.get_payload(decode=True)
                                file_path = "audio.wav"
                                with open(file_path, "wb") as audio_file:
                                    audio_file.write(audio_content)

                                segment_duration_ms = 59000  
                                segments = split_audio(file_path, segment_duration_ms)
                                combined_text = await process_and_combine_segments(segments)
                                
                                if len(combined_text) > 1024:
                                    text_parts = split_text(combined_text, 1024)
                                else:
                                    text_parts = [combined_text]

                                telegram_message = f"Subject: {subject}\nEmail Text: {email_text}\n\nTranscription: "
                                await send_telegram_message_async([telegram_message + part for part in text_parts], file_path)
                        mail.store(email_id, "+FLAGS", "\\Seen")
                    except Exception as e:
                        logging.error(f"Error processing email ID {email_id}: {e}")
            else:
                if os.path.exists("audio.wav"):
                    os.remove("audio.wav")

        except imaplib.IMAP4.abort as e:
            logging.error(f"IMAP connection error: {e}")
        except imaplib.IMAP4.error as e:
            logging.error(f"IMAP error: {e}")
        except Exception as e:
            logging.error(f"Error in main_async loop: {e}")
        
        await asyncio.sleep(10)

if __name__ == "__main__":
    logging.info("Starting main_async loop.")
    loop = asyncio.get_event_loop()
    if loop.is_running():
        loop.create_task(main_async())
    else:
        asyncio.run(main_async())