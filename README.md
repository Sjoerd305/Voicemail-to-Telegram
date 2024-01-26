
# Voicemail to Telegram

Retrieves emails using IMAP, extracts the text and voicemail message. Parses the voicemail to Google Text to Speech API and parses the email text and transcripted voicemail text to a Telegram message with the voicemail attached.

## Dependencies 
- pip install google-cloud-speech pydub schedule paramiko
- pip install --user python-telegram-bot==12.8
- apt install ffmpeg

## Google Cloud SDK
- Enable Google Text to Speech API
- Google Service Account API key (googlekey.json)

## Docker

- Dockerfile
- docker run --name voicemailapp -d --restart unless-stopped -v "map directory with your config files":/config sj0erd/voicemailapp:google


# main_script_v2.py

Redefined main script, does not use Google Cloud Speech to Text API, instead it uses OpenAI Whisper module in python. Does require a minimum of 6GB of RAM and fast processor to transcribe the messages to Telegram. Transcribing messages using Whisper take longer but quality is better that Google Speech to Text API, currently testing on whsiper model medium. The large model is seems to be a bit better fot the dutch language, but processing times are too long for actual use. 

Pro's
- Transcribes audio files on local machine, no cloud needed
- Better transcriptions
- Main script is revised
- Support for longer audio files
- Support for longer transcriptions and multiple telegram messages
- No fees

Cons
- Slower
- Needs a heavier machine
