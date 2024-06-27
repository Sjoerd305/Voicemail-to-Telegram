FROM python:3.11

# Install dependencies
RUN apt-get update && apt-get install -y \
    ffmpeg \
    apt-transport-https \
    ca-certificates \
    gnupg \
    curl \
    sudo \
    && apt-get clean

# Install Python packages
RUN python -m pip install --upgrade pip && \
    python -m pip install google-cloud-speech pydub schedule paramiko requests nest_asyncio python-telegram-bot==21.3

# Add Google Cloud SDK source and install
RUN echo "deb [signed-by=/usr/share/keyrings/cloud.google.gpg] http://packages.cloud.google.com/apt cloud-sdk main" \
    | tee -a /etc/apt/sources.list.d/google-cloud-sdk.list && \
    curl https://packages.cloud.google.com/apt/doc/apt-key.gpg | apt-key --keyring /usr/share/keyrings/cloud.google.gpg add - && \
    apt-get update && apt-get install -y google-cloud-cli

# Copy application files
COPY main.py /config/googlekey.json main.sh telegram_listener.py emailcleanup.py .

# Set environment variables
ENV TZ="Europe/Amsterdam"

# Ensure main.sh is executable
RUN chmod +x main.sh

# Activate Google Cloud API
RUN gcloud auth activate-service-account voicemail-app@voicemail-396410.iam.gserviceaccount.com --key-file=googlekey.json --project=voicemail-396410

RUN rm googlekey.json

# Start the application
CMD ["bash", "main.sh"]
