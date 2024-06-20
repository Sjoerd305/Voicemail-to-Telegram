import logging
import paramiko
import configparser
import requests
from telegram import Update, Bot
from telegram.ext import Updater, CommandHandler, MessageHandler, Filters, CallbackContext, run_async
from pathlib import Path
import json

# Define the configuration file paths
config_dir = Path(__file__).resolve().parent / 'config'
config_file = config_dir / 'config.ini'
phone_numbers_file = config_dir / 'phone_numbers.ini'
info_file = config_dir / 'info.txt'
customers_file = config_dir / 'customers.json'

# Check if configuration files exist
if not config_file.is_file() or not phone_numbers_file.is_file() or not info_file.is_file() or not customers_file.is_file():
    raise FileNotFoundError("One or more configuration files are missing.")

# Read configuration
config = configparser.ConfigParser()
config.read(config_file)
config.read(phone_numbers_file)

# Set up logging
logging.basicConfig(
    level=logging.ERROR,
    format='%(asctime)s - %(levelname)s - %(message)s',
    handlers=[
        logging.StreamHandler(),
        logging.FileHandler("Listener.log")
    ]
)

# Extract configuration values
try:
    TELEGRAM_TOKEN = config['secrets']['TELEGRAMTOKEN']
    PBXIP = config['secrets']['PBXIP']
    PHONE_NUMBERS = dict(config['PhoneNumbers'])
except KeyError as e:
    logging.error("Missing configuration value: %s", e)
    raise

# General function for setting storingsdienst
def set_storingsdienst(name: str, phone_number: str, update: Update, context: CallbackContext) -> None:
    chat_id = update.effective_chat.id
    try:
        response = requests.get(f"http://{PBXIP}/storingsdienst/setnummer.php?setnummer={phone_number}")
        if response.status_code == 200:
            update.message.reply_text(f"Storingsdienst naar {name}")
        else:
            update.message.reply_text("Er is een fout opgetreden.")
            logging.error("Failed to set storingsdienst for %s: %s", name, response.text)
    except requests.RequestException as e:
        update.message.reply_text("Er is een fout opgetreden.")
        logging.error("Exception while setting storingsdienst for %s: %s", name, e)

def handle_storingsdienst_command(update: Update, context: CallbackContext) -> None:
    try:
        command = update.message.text[1:].split('@')[0]  # Extract the command name
        name = command.capitalize()
        phone_number = PHONE_NUMBERS.get(name.lower())
        if phone_number:
            set_storingsdienst(name, phone_number, update, context)
        else:
            update.message.reply_text("Onbekend commando.")
            logging.warning("Unknown command received: %s", command)
    except Exception as e:
        update.message.reply_text("Er is een fout opgetreden bij het verwerken van het commando.")
        logging.error("Error handling storingsdienst command: %s", e)

def execute_ssh_command(command: str, success_message: str, error_message: str, chat_id: int, bot: Bot) -> None:
    ssh_client = paramiko.SSHClient()
    ssh_client.load_system_host_keys()
    ssh_client.set_missing_host_key_policy(paramiko.AutoAddPolicy())

    try:
        ssh_client.connect(config['secrets']['SSH_HOST'], 
                           int(config['secrets']['SSH_PORT']), 
                           config['secrets']['SSH_USERNAME'], 
                           config['secrets']['SSH_PASSWORD'])

        stdin, stdout, stderr = ssh_client.exec_command(command)
        error_output = stderr.read()
        if error_output:
            bot.send_message(chat_id=chat_id, text=error_message)
            logging.error("SSH command error: %s", error_output)
        else:
            bot.send_message(chat_id=chat_id, text=success_message)
    except Exception as e:
        bot.send_message(chat_id=chat_id, text=f"Error: {str(e)}")
        logging.error("Exception during SSH command execution: %s", e)
    finally:
        ssh_client.close()

@run_async
def delete_vm(update: Update, context: CallbackContext) -> None:
    if update.message.chat.type == "group":
        chat_id = update.message.chat_id
        execute_ssh_command("rm -f /var/spool/asterisk/voicemail/default/9001/INBOX/*.*",
                            "Voicemail verwijderd.",
                            "Probleem bij verwijderen voicemail.",
                            chat_id, context.bot)

@run_async
def vivia(update: Update, context: CallbackContext) -> None:
    if update.message.chat.type == "group":
        chat_id = update.message.chat_id
        execute_ssh_command("/var/lib/misc/vivia/vivia.sh",
                            "Storingsdienst naar Vivia",
                            "Probleem bij omzetten storingsdienst",
                            chat_id, context.bot)

@run_async
def avics(update: Update, context: CallbackContext) -> None:
    if update.message.chat.type == "group":
        chat_id = update.message.chat_id
        execute_ssh_command("/var/lib/misc/avics/avics.sh",
                            "Storingsdienst naar Avics",
                            "Probleem bij omzetten storingsdienst",
                            chat_id, context.bot)

def info(update: Update, context: CallbackContext):
    if update.message.chat.type == "group":
        try:
            with open(info_file, "r") as file:
                info = file.read()
            update.message.reply_text(info)
        except Exception as e:
            update.message.reply_text("Er is een fout opgetreden bij het ophalen van de informatie.")
            logging.error("Error reading info file: %s", e)

def handle_customer_command(update: Update, context: CallbackContext) -> None:
    command = update.message.text[1:].lower()  # Extract the command name
    # Load customer information dynamically from JSON file
    with open(customers_file, 'r') as file:
        CUSTOMERS = json.load(file)
    customer_info = CUSTOMERS.get(command)
    if customer_info:
        formatted_message = customer_info.replace('\n', '\n')
        update.message.reply_text(formatted_message)
    else:
        update.message.reply_text("Onbekend commando, zie /info of /klant")
        logging.warning("Unknown customer command received: %s", command)

def lol(update: Update, context: CallbackContext) -> None:
    if update.message.chat.type == "group":
        chat_id = update.message.chat_id
        context.bot.send_message(chat_id=chat_id, text="https://www.youtube.com/watch?v=dQw4w9WgXcQ")

def handle_other_messages(update: Update, context: CallbackContext) -> None:
    pass

def main() -> None:
    try:
        # Initialize the Telegram Bot
        bot = Bot(token=TELEGRAM_TOKEN)
        updater = Updater(bot=bot, use_context=True, request_kwargs={'con_pool_size': 10})
        dispatcher = updater.dispatcher

        # Register the command handlers
        dispatcher.add_handler(CommandHandler("deletevm", delete_vm))
        dispatcher.add_handler(CommandHandler("vivia", vivia))
        dispatcher.add_handler(CommandHandler("avics", avics))
        dispatcher.add_handler(CommandHandler(list(PHONE_NUMBERS.keys()), handle_storingsdienst_command))
        dispatcher.add_handler(CommandHandler("info", info))
        dispatcher.add_handler(CommandHandler("lol", lol))

        # Register a generic handler for all customer commands
        dispatcher.add_handler(MessageHandler(Filters.command, handle_customer_command))

        # Register a message handler for all other text messages in the group chat
        dispatcher.add_handler(MessageHandler(Filters.text & ~Filters.command, handle_other_messages))

        # Start listening for updates
        updater.start_polling()
        updater.idle()

    except Exception as e:
        logging.error("Unhandled exception in main: %s", e)
        raise

if __name__ == '__main__':
    main()
