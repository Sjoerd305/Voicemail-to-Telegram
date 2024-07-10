import schedule
import time
import imaplib
import email
import configparser
from datetime import datetime
from pathlib import Path
import logging

# Set up logging directory and file
log_dir = Path(__file__).resolve().parent / 'logs'
log_dir.mkdir(exist_ok=True)
log_file = log_dir / 'email_cleanup.log'

# Set up logging
logging.basicConfig(
    level=logging.ERROR,
    format='%(asctime)s - %(levelname)s - %(message)s',
    handlers=[
        logging.StreamHandler(),
        logging.FileHandler(log_file)
    ]
)

config_dir = Path(__file__).resolve().parent / 'config'
config_file = config_dir / 'config.ini'
config = configparser.ConfigParser()
config.read(config_file)

# Configuration for the email account and IMAP server
imap_host = config['secrets']['IMAPSERVER']
imap_user = config['secrets']['EMAIL']
imap_pass = config['secrets']['PASSWORD']

def get_current_week_range():
    current_week = datetime.now().isocalendar()
    current_year = int(datetime.now().year)
    folder_name = f"INBOX.{current_year}.{current_week[1] - 1}-{current_week[1]}"
    return folder_name, current_week[0]

def process_emails():
    try:
        mail = imaplib.IMAP4_SSL(imap_host)
        mail.login(imap_user, imap_pass)
        mail.select('INBOX')

        folder_name, _ = get_current_week_range()
        status, folder_list = mail.list()

        inbox_folder_name = f'{folder_name}'
        if inbox_folder_name.encode() not in [f.split()[-1] for f in folder_list[1]]:
            status, create_response = mail.create(inbox_folder_name)
            if status != 'OK':
                logging.error(f"Failed to create folder: {inbox_folder_name}, Status: {status}, Response: {create_response}")
                mail.logout()
                return
            logging.info(f"Created folder: {inbox_folder_name}")

        status, messages = mail.uid('search', None, 'ALL')
        if status == 'OK':
            messages = messages[0].split()
            for mail_id in messages:
                logging.info(f"Processing email with UID: {mail_id}")
                status, move_response = mail.uid('COPY', mail_id, f'"{inbox_folder_name}"')
                if status == 'OK':
                    mail.uid('STORE', mail_id, '+FLAGS', '\\Deleted')
                    logging.info(f"Moved email UID: {mail_id} to folder: {inbox_folder_name}")
                else:
                    logging.error(f"Failed to move email {mail_id} to folder {inbox_folder_name}, Status: {status}, Response: {move_response}")

        mail.expunge()
        logging.info("Expunged deleted emails from INBOX")

        mail.close()
        mail.logout()
    except Exception as e:
        logging.error(f"An error occurred: {str(e)}")

def job():
    process_emails()

# Schedule the job to run every Friday at 09:00
schedule.every().friday.at("09:00").do(job)

while True:
    schedule.run_pending()
    time.sleep(10)
