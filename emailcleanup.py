import schedule
import time
import imaplib
import email
import configparser
from datetime import datetime
from pathlib import Path


config_dir = Path(__file__).resolve().parent / 'config'
config_file = config_dir / 'config.ini'
config = configparser.ConfigParser()
config.read(config_file)

# Configuration for the email account and IMAP server
imap_host = config['secrets']['IMAPSERVER']
imap_user = config['secrets']['EMAIL']
imap_pass = config['secrets']['PASSWORD']

def get_current_week_range():
    # Get the current ISO week date (year, week number, and weekday)
    current_week = datetime.now().isocalendar()
    current_year = int(datetime.now().year)

    # Determine the folder name in the format "previous_week-current_week"
    folder_name = f"INBOX.{current_year}.{current_week[1] - 1}-{current_week[1]}"

    # Return the folder name
    return folder_name, current_week[0]


def process_emails():
    # Connect to the IMAP server
    mail = imaplib.IMAP4_SSL(imap_host)
    mail.login(imap_user, imap_pass)
    mail.select('INBOX')

    # Determine the folder name based on the current week
    folder_name, _ = get_current_week_range()  # Get the folder name and ignore the current year for now

    # List all folders
    status, folder_list = mail.list()

    # Check if the target folder already exists
    inbox_folder_name = f'{folder_name}'
    if inbox_folder_name.encode() not in folder_list[1]:
        # If the folder doesn't exist, create it
        status, create_response = mail.create(inbox_folder_name)
        if status != 'OK':
            # Handle folder creation failure
            print(f"Failed to create folder: {inbox_folder_name}")
            print(f"Status: {status}, Response: {create_response}")
            mail.logout()
            return

    # Move all emails to the folder
    status, messages = mail.uid('search', None, 'ALL')
    if status == 'OK':
        messages = messages[0].split()
        for mail_id in messages:
            print(f"Processing email with UID: {mail_id}")
            status, move_response = mail.uid('COPY', mail_id, f'"{inbox_folder_name}"')
            if status == 'OK':
                # Mark the email as deleted in INBOX
                mail.uid('STORE', mail_id, '+FLAGS', '\\Deleted')
            else:
                # Handle email move failure
                print(f"Failed to move email {mail_id} to folder {inbox_folder_name}")
                print(f"Status: {status}, Response: {move_response}")

    # Expunge to permanently remove all emails marked for deletion from the INBOX
    mail.expunge()

    # Close the mailbox and logout
    mail.close()
    mail.logout()

def job():
    process_emails()

# Schedule the job to run every Friday at 09:00
schedule.every().friday.at("09:00").do(job)

while True:
    schedule.run_pending()
    time.sleep(10)
