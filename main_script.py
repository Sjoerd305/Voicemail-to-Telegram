import asyncio
import imaplib
import email
import os
import configparser
import telegram
from telegram import Bot
from telegram.error import TelegramError
from google.cloud import speech
from google.oauth2 import service_account
from pydub import AudioSegment

config = configparser.ConfigParser()
config.read('/config/config.ini')

IMAP_SERVER = config['secrets']['IMAPSERVER']
IMAP_PORT = config['secrets']['IMAPPORT']
EMAIL = config['secrets']['EMAIL']
PASSWORD = config['secrets']['PASSWORD']
MAX_CAPTION_LENGTH = 1024
TELEGRAM_TOKEN = config['secrets']['TELEGRAMTOKEN']
CHAT_ID = config['secrets']['CHATID']

#Google API
credentials = service_account.Credentials.from_service_account_file('googlekey.json')
client = speech.SpeechClient(credentials=credentials)

def split_text(text, max_length):
    """Splits a text into parts where each part is at most `max_length` characters."""
    parts = []
    while text:
        # Take the first `max_length` characters from the text
        part = text[:max_length]
        parts.append(part)
        # Remove the part we just added from the text
        text = text[max_length:]
    return parts

async def convert_speech_to_text_inline(audio_content):

    language_code = "nl-NL"
    sample_rate_hertz = 8000

    audio = speech.RecognitionAudio(content=audio_content)
    config = speech.RecognitionConfig(
        encoding=speech.RecognitionConfig.AudioEncoding.LINEAR16,
        sample_rate_hertz=sample_rate_hertz,
        language_code=language_code,
    )

    response = client.recognize(config=config, audio=audio)

    transcript = ""
    for result in response.results:
        transcript += result.alternatives[0].transcript

    return transcript

def split_audio(audio_path, segment_duration_ms):
    audio = AudioSegment.from_file(audio_path)
    segments = []
    
    start = 0
    while start < len(audio):
        end = start + segment_duration_ms
        segment = audio[start:end]
        segments.append(segment)
        start = end
    
    return segments

async def process_and_combine_segments(segments):
    combined_transcription = ""

    for segment in segments:
        text = await convert_speech_to_text_inline(segment.raw_data)
        combined_transcription += text + " "  # Add a space between segment transcriptions

    return combined_transcription.strip()  # Remove trailing space

async def send_telegram_message_async(texts, audio_path):
    """
    Send Telegram messages with an audio file. If a single message exceeds the maximum caption length,
    split it into multiple parts and send each part as a separate message with the audio attached.
    """
    try:
        bot = Bot(token=TELEGRAM_TOKEN)
        for text in texts:
            # Split long text into multiple parts
            text_parts = split_text(text, MAX_CAPTION_LENGTH - 20)  # Leave room for "Part x/y: " prefix
            for i, part in enumerate(text_parts, start=1):
                caption = f"Part {i}/{len(text_parts)}: " + part
                # Re-open the audio file for each message part
                with open(audio_path, 'rb') as audio_file:
                    bot.send_voice(chat_id=CHAT_ID, voice=audio_file, caption=caption, parse_mode="Markdown")
    except TelegramError as e:
        print("TelegramError:", e)

async def main_async():
    while True:
        try:
            mail = imaplib.IMAP4_SSL(IMAP_SERVER, IMAP_PORT)
            mail.login(EMAIL, PASSWORD)
            mail.select("inbox")

            result, data = mail.search(None, 'UNSEEN SUBJECT "PBX"') #Look for new emails with matching criteria. 
            email_ids = data[0].split()

            if email_ids:
                for email_id in email_ids:
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
                            
                            # Check if combined_text is too long
                            if len(combined_text) > MAX_CAPTION_LENGTH:
                                text_parts = split_text(combined_text, MAX_CAPTION_LENGTH)
                            else:
                                text_parts = [combined_text]

                            telegram_message = f"Subject: {subject}\nEmail Text: {email_text}\nTranscription: "
                            await send_telegram_message_async([telegram_message + part for part in text_parts], file_path)
                    mail.store(email_id, "+FLAGS", "\\Seen")
            else:
                if os.path.exists("audio.wav"):
                    os.remove("audio.wav")

        except Exception as e:
            print("Error:", e)

        await asyncio.sleep(60)

if __name__ == "__main__":

    asyncio.run(main_async())