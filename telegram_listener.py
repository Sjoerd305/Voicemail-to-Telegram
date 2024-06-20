import logging
import paramiko
import configparser
import requests
from telegram import Update, Bot
from telegram.ext import Application, CommandHandler, MessageHandler, filters, ContextTypes
from pathlib import Path
import json
import asyncio
import nest_asyncio

# Apply the nest_asyncio patch
nest_asyncio.apply()

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
async def set_storingsdienst(name: str, phone_number: str, update: Update, context: ContextTypes.DEFAULT_TYPE) -> None:
    chat_id = update.effective_chat.id
    try:
        response = requests.get(f"http://{PBXIP}/storingsdienst/setnummer.php?setnummer={phone_number}")
        if response.status_code == 200:
            await update.message.reply_text(f"Storingsdienst naar {name}")
        else:
            await update.message.reply_text("Er is een fout opgetreden.")
            logging.error("Failed to set storingsdienst for %s: %s", name, response.text)
    except requests.RequestException as e:
        await update.message.reply_text("Er is een fout opgetreden.")
        logging.error("Exception while setting storingsdienst for %s: %s", name, e)

async def handle_storingsdienst_command(update: Update, context: ContextTypes.DEFAULT_TYPE) -> None:
    try:
        command = update.message.text[1:].split('@')[0]  # Extract the command name
        name = command.capitalize()
        phone_number = PHONE_NUMBERS.get(name.lower())
        if phone_number:
            await set_storingsdienst(name, phone_number, update, context)
        else:
            await update.message.reply_text("Onbekend commando.")
            logging.warning("Unknown command received: %s", command)
    except Exception as e:
        await update.message.reply_text("Er is een fout opgetreden bij het verwerken van het commando.")
        logging.error("Error handling storingsdienst command: %s", e)

async def execute_ssh_command(command: str, success_message: str, error_message: str, chat_id: int, bot: Bot) -> None:
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
            await bot.send_message(chat_id=chat_id, text=error_message)
            logging.error("SSH command error: %s", error_output)
        else:
            await bot.send_message(chat_id=chat_id, text=success_message)
    except Exception as e:
        await bot.send_message(chat_id=chat_id, text=f"Error: {str(e)}")
        logging.error("Exception during SSH command execution: %s", e)
    finally:
        ssh_client.close()

async def delete_vm(update: Update, context: ContextTypes.DEFAULT_TYPE) -> None:
    if update.message.chat.type == "group":
        chat_id = update.message.chat_id
        await execute_ssh_command("rm -f /var/spool/asterisk/voicemail/default/9001/INBOX/*.*",
                                  "Voicemail verwijderd.",
                                  "Probleem bij verwijderen voicemail.",
                                  chat_id, context.bot)

async def vivia(update: Update, context: ContextTypes.DEFAULT_TYPE) -> None:
    if update.message.chat.type == "group":
        chat_id = update.message.chat_id
        await execute_ssh_command("/var/lib/misc/vivia/vivia.sh",
                                  "Storingsdienst naar Vivia",
                                  "Probleem bij omzetten storingsdienst",
                                  chat_id, context.bot)

async def avics(update: Update, context: ContextTypes.DEFAULT_TYPE) -> None:
    if update.message.chat.type == "group":
        chat_id = update.message.chat_id
        await execute_ssh_command("/var/lib/misc/avics/avics.sh",
                                  "Storingsdienst naar Avics",
                                  "Probleem bij omzetten storingsdienst",
                                  chat_id, context.bot)

async def info(update: Update, context: ContextTypes.DEFAULT_TYPE):
    if update.message.chat.type == "group":
        try:
            with open(info_file, "r") as file:
                info = file.read()
            await update.message.reply_text(info)
        except Exception as e:
            await update.message.reply_text("Er is een fout opgetreden bij het ophalen van de informatie.")
            logging.error("Error reading info file: %s", e)

async def handle_customer_command(update: Update, context: ContextTypes.DEFAULT_TYPE) -> None:
    command = update.message.text[1:].lower()  # Extract the command name
    # Load customer information dynamically from JSON file
    with open(customers_file, 'r') as file:
        CUSTOMERS = json.load(file)
    customer_info = CUSTOMERS.get(command)
    if customer_info:
        formatted_message = customer_info.replace('\n', '\n')
        await update.message.reply_text(formatted_message)
    else:
        await update.message.reply_text("Onbekend commando, zie /info of /klant")
        logging.warning("Unknown customer command received: %s", command)

async def lol(update: Update, context: ContextTypes.DEFAULT_TYPE) -> None:
    if update.message.chat.type == "group":
        chat_id = update.message.chat_id
        await context.bot.send_message(chat_id=chat_id, text="https://www.youtube.com/watch?v=dQw4w9WgXcQ")

async def handle_other_messages(update: Update, context: ContextTypes.DEFAULT_TYPE) -> None:
    pass

async def main() -> None:
    try:
        # Initialize the Telegram Bot
        bot = Bot(token=TELEGRAM_TOKEN)
        application = Application.builder().token(TELEGRAM_TOKEN).build()
        
        # Initialize application
        await application.initialize()
        
        # Register the command handlers
        application.add_handler(CommandHandler("deletevm", delete_vm))
        application.add_handler(CommandHandler("vivia", vivia))
        application.add_handler(CommandHandler("avics", avics))
        for cmd in PHONE_NUMBERS.keys():
            application.add_handler(CommandHandler(cmd, handle_storingsdienst_command))
        application.add_handler(CommandHandler("info", info))
        application.add_handler(CommandHandler("lol", lol))

        # Register a generic handler for all customer commands
        application.add_handler(MessageHandler(filters.COMMAND, handle_customer_command))

        # Register a message handler for all other text messages in the group chat
        application.add_handler(MessageHandler(filters.TEXT & ~filters.COMMAND, handle_other_messages))

        # Start polling for updates
        await application.run_polling()

    except Exception as e:
        logging.error("Unhandled exception in main: %s", e)
        raise

if __name__ == '__main__':
    loop = asyncio.get_event_loop()
    if loop.is_running():
        loop.create_task(main())
    else:
        asyncio.run(main())
