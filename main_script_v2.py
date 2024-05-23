import asyncio
import imaplib
import email
import os
import configparser
import logging
from telegram import Bot, TelegramError
import whisper

# Set up logging
logging.basicConfig(
    filename='debug.log', 
    level=logging.DEBUG, 
    format='%(asctime)s %(levelname)s:%(message)s'
)

# Read configuration
config = configparser.ConfigParser()
config.read('config.ini')
model = whisper.load_model("base")  # Define OpenAI Whisper model to be used, options: base, small, medium, large

IMAP_SERVER = config['secrets']['IMAPSERVER']
IMAP_PORT = config['secrets']['IMAPPORT']
EMAIL = config['secrets']['EMAIL']
PASSWORD = config['secrets']['PASSWORD']
TELEGRAM_TOKEN = config['secrets']['TELEGRAMTOKEN']
CHAT_ID = config['secrets']['CHATID']


async def send_voice_message(bot, chat_id, audio_path, caption):
    """
    Sends a voice message in smaller parts with captions.

    Args:
        bot (telegram.Bot): The Telegram Bot instance.
        chat_id (str or int): The chat ID where the message will be sent.
        audio_path (str): The file path of the audio to be sent.
        caption (str): The caption text to accompany the audio.

    Raises:
        TelegramError: If there's an error sending a part of the message via Telegram.
        Exception: For any other general errors.
    """
    # Split the caption into smaller parts
    message_parts = [caption[i:i + 1024] for i in range(0, len(caption), 1024)]

    for index, part in enumerate(message_parts):
        try:
            with open(audio_path, 'rb') as audio_file:
                await bot.send_voice(chat_id=chat_id, voice=audio_file, caption=part)
            await asyncio.sleep(2)  # Delay to avoid overloading Telegram
        except TelegramError as e:
            logging.error(f"TelegramError on part {index+1}: {e}")
        except Exception as e:
            logging.error(f"General Error on part {index+1}: {e}")

def convert_speech_to_text(audio_path):
    """Converts speech in an audio file to text using Whisper."""
    if not os.path.exists(audio_path) or os.path.getsize(audio_path) == 0:
        logging.debug(f"Audio file {audio_path} is missing or empty.")
        return "Transcription failed due to missing or empty audio file."
    try:
        logging.debug(f"Starting transcription for {audio_path}.")
        result = model.transcribe(audio_path)["text"]
        logging.debug(f"Transcription result: {result}")
        return result
    except Exception as e:
        logging.error(f"Transcription failed: {e}")
        return f"Transcription failed: {e}"

async def main_async():
    """Main asynchronous loop checking emails and sending Telegram messages."""
    bot = Bot(token=TELEGRAM_TOKEN)
    while True:
        try:
            logging.debug("Connecting to the mail server.")
            mail = imaplib.IMAP4_SSL(IMAP_SERVER, IMAP_PORT)
            mail.login(EMAIL, PASSWORD)
            logging.debug("Logged in to the mail server.")
            mail.select("inbox")
            result, data = mail.search(None, 'UNSEEN SUBJECT "PBX"')
            email_ids = data[0].split()
            logging.debug(f"Found {len(email_ids)} emails.")
            for email_id in email_ids:
                result, email_data = mail.fetch(email_id, "(RFC822)")
                logging.debug(f"Fetching email ID {email_id}: {result}")
                raw_email = email_data[0][1]
                msg = email.message_from_bytes(raw_email)
                subject = msg["subject"]
                email_text = "".join(part.get_payload(decode=True).decode("utf-8") for part in msg.walk() if part.get_content_type() == "text/plain")
                audio_path = "audio.wav"  # Assumes the input file is a .wav file, saves it under one filename for easy deletion.
                for part in msg.walk():
                    if part.get_content_type().startswith("audio/"):  # Look for any audio file in the email attachment.
                        logging.debug(f"Found audio attachment: {part.get_content_type()}")
                        with open(audio_path, "wb") as audio_file:
                            audio_file.write(part.get_payload(decode=True))
                
                transcription = convert_speech_to_text(audio_path)
                telegram_message = f"Subject: {subject}\nEmail Text: {email_text}\nTranscription: {transcription}"
                
                # Send the voice message in smaller parts with captions
                await send_voice_message(bot, CHAT_ID, audio_path, telegram_message)
                
                mail.store(email_id, "+FLAGS", "\\Seen")
                logging.debug(f"Marked email ID {email_id} as seen.")

            else:
                if os.path.exists("audio.wav"):
                    os.remove("audio.wav")
                    logging.debug("Deleted temporary audio file.")

        except Exception as e:
            logging.error(f"Error: {e}")

        await asyncio.sleep(10)

if __name__ == "__main__":
    logging.debug("Starting the main async loop.")
    asyncio.run(main_async())
