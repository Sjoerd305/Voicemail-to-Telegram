
# Voicemail to Telegram

Retrieves emails using IMAP, extracts the text and voicemail message. Parses the voicemail to Google Text to Speech API and parses the email text and transcripted voicemail text to a Telegram message with the voicemail attached.

## Dependencies 
- pip install google-cloud-speech pydub schedule paramiko
- pip install --user python-telegram-bot==21.3
- apt install ffmpeg

## Google Cloud SDK
- Enable Google Text to Speech API
- Google Service Account API key (googlekey.json)

## Docker

- Dockerfile
- docker run --name voicemailapp -d --restart unless-stopped -v "map directory with your config files":/config sj0erd/voicemailapp:google
