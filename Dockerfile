FROM python:3.11
#Dependencies for Voicemail app
RUN apt-get update && apt-get install -y ffmpeg && apt-get clean
RUN pip install google-cloud-speech pydub schedule paramiko requests
RUN python -m pip install --user python-telegram-bot==12.8

#Google Cloud dependencies
RUN apt-get install -y apt-transport-https ca-certificates gnupg curl sudo
RUN echo "deb [signed-by=/usr/share/keyrings/cloud.google.gpg] http://packages.cloud.google.com/apt cloud-sdk main" | tee -a /etc/apt/sources.list.d/google-cloud-sdk.list && curl https://packages.cloud.google.com/apt/doc/apt-key.gpg | apt-key --keyring /usr/share/keyrings/cloud.google.gpg  add - && apt-get update -y && apt-get install google-cloud-cli -y

COPY main.py /config/googlekey.json main.sh telegram_listener.py emailcleanup.py .

#Enviroment
ENV TZ="Europe/Amsterdam"

#Activate Google Cloud API
RUN gcloud auth activate-service-account voicemail-app@voicemail-396410.iam.gserviceaccount.com --key-file=googlekey.json --project=voicemail-396410

CMD ["bash", "main.sh"]