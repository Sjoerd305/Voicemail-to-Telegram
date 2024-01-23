import asyncio
import imaplib
import email
import os
import configparser
from telegram import Bot, TelegramError
import whisper


# Read configuration
config = configparser.ConfigParser()
config.read('/config/config.ini')
model = whisper.load_model("medium")  # Define OpenAI Whisper model to be used, optiens: base, small, medium, large
IMAP_SERVER = config['secrets']['IMAPSERVER']
IMAP_PORT = config['secrets']['IMAPPORT']
EMAIL = config['secrets']['EMAIL']
PASSWORD = config['secrets']['PASSWORD']
TELEGRAM_TOKEN = config['secrets']['TELEGRAMTOKEN']
CHAT_ID = config['secrets']['CHATID']
MAX_CAPTION_LENGTH = 1024  # Telegram's max caption length for voice messages

def split_message(text, max_length):
    """Splits a text into chunks of a maximum length."""
    return [text[i:i + max_length] for i in range(0, len(text), max_length)]

async def send_voice_message(bot, chat_id, audio_path, caption):
    """Sends a voice message in smaller parts with captions."""
    message_parts = split_message(caption, MAX_CAPTION_LENGTH)

    for index, part in enumerate(message_parts):
        try:
            with open(audio_path, 'rb') as audio_file:
                await bot.send_voice(chat_id=chat_id, voice=audio_file, caption=part)
            await asyncio.sleep(2)  # Delay, don't overload Telegram
        except TelegramError as e:
            print(f"TelegramError on part {index+1}: {e}")  # Debugging line
        except Exception as e:
            print(f"General Error on part {index+1}: {e}")  # Debugging line


def convert_speech_to_text(audio_path):
    """Converts speech in an audio file to text using Whisper."""
    if not os.path.exists(audio_path) or os.path.getsize(audio_path) == 0:
        return "Transcription failed due to missing or empty audio file."
    try:
        return model.transcribe(audio_path)["text"]
    except Exception as e:
        return f"Transcription failed: {e}"

async def main_async():
    """Main asynchronous loop checking emails and sending Telegram messages."""
    bot = Bot(token=TELEGRAM_TOKEN)
    while True:
        try:
            mail = imaplib.IMAP4_SSL(IMAP_SERVER, IMAP_PORT)
            mail.login(EMAIL, PASSWORD)
            mail.select("inbox")
            result, data = mail.search(None, 'UNSEEN SUBJECT "PBX"')
            email_ids = data[0].split()
            for email_id in email_ids:
                result, email_data = mail.fetch(email_id, "(RFC822)")
                raw_email = email_data[0][1]
                msg = email.message_from_bytes(raw_email)
                subject = msg["subject"]
                email_text = "".join(part.get_payload(decode=True).decode("utf-8") for part in msg.walk() if part.get_content_type() == "text/plain")
                audio_path = "audio.wav" #Assumes the input file is a .wav file, saves it under one filename for easy deletion.
                for part in msg.walk():
                    if part.get_content_type().startswith("audio/"): #Look for any audio file in the email attachement. 
                        with open(audio_path, "wb") as audio_file:
                            audio_file.write(part.get_payload(decode=True))
                            
                transcription = convert_speech_to_text(audio_path)
                telegram_message = f"Subject: {subject}\nEmail Text: {email_text}\nTranscription: {transcription}"
                
                # Send the voice message in smaller parts with captions
                await send_voice_message(bot, CHAT_ID, audio_path, telegram_message)
                
                mail.store(email_id, "+FLAGS", "\\Seen")

            else:
                if os.path.exists("audio.wav"):
                    os.remove("audio.wav")

        except Exception as e:
            print("Error:", e)

        await asyncio.sleep(60)

if __name__ == "__main__":
    asyncio.run(main_async())
