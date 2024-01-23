import logging
import paramiko
import configparser
import requests
from telegram import Update, Bot
from telegram.ext import Updater, CommandHandler, MessageHandler, Filters, CallbackContext, run_async

# Read configuration
config = configparser.ConfigParser()
config.read('/config/config.ini')
config.read('/config/phone_numbers.ini')

# Set up logging
logging.basicConfig(format='%(asctime)s - %(name)s - %(levelname)s - %(message)s', level=logging.INFO)
logger = logging.getLogger(__name__)

# Extract configuration values
TELEGRAM_TOKEN = config['secrets']['TELEGRAMTOKEN']
PBXIP = config['secrets']['PBXIP']

PHONE_NUMBERS = dict(config['PhoneNumbers'])

# General function for setting storingsdienst
def set_storingsdienst(name: str, phone_number: str, update: Update, context: CallbackContext) -> None:
    chat_id = update.effective_chat.id
    response = requests.get(f"http://{PBXIP}/storingsdienst/setnummer.php?setnummer={phone_number}")
    if response.status_code == 200:
        update.message.reply_text(f"Storingsdienst naar {name}")
    else:
        update.message.reply_text("Er is een fout opgetreden.")

# Function to handle storingsdienst commands
def handle_storingsdienst_command(update: Update, context: CallbackContext) -> None:
    command = update.message.text[1:].split('@')[0]  # Extract the command name
    name = command.capitalize()
    phone_number = PHONE_NUMBERS.get(name.lower())
    if phone_number:
        set_storingsdienst(name, phone_number, update, context)
    else:
        update.message.reply_text("Onbekend commando.")

def execute_ssh_command(command: str, success_message: str, error_message: str, chat_id: int, bot: Bot) -> None:
    ssh_client = paramiko.SSHClient()
    ssh_client.load_system_host_keys()
    ssh_client.set_missing_host_key_policy(paramiko.AutoAddPolicy())

    try:
        ssh_client.connect(config['secrets']['SSH_HOST'], 
                           config['secrets']['SSH_PORT'], 
                           config['secrets']['SSH_USERNAME'], 
                           config['secrets']['SSH_PASSWORD'])

        stdin, stdout, stderr = ssh_client.exec_command(command)

        if stderr.read():
            bot.send_message(chat_id=chat_id, text=error_message)
        else:
            bot.send_message(chat_id=chat_id, text=success_message)
    except Exception as e:
        bot.send_message(chat_id=chat_id, text=f"Error: {str(e)}")
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


def vivia(update: Update, context: CallbackContext) -> None:
    if update.message.chat.type == "group":
        chat_id = update.message.chat_id
        execute_ssh_command("/var/lib/misc/vivia/vivia.sh")

def avics(update: Update, context: CallbackContext) -> None:
    if update.message.chat.type == "group":
        chat_id = update.message.chat_id
        execute_ssh_command("/var/lib/misc/avics/avics.sh")


def info(update: Update, context: CallbackContext):
    if update.message.chat.type == "group":
        with open("info.txt", "r") as file:
            info = file.read()
        update.message.reply_text(f"Beschikbare commando's: \n \n{info}")

# Function to handle other messages
def handle_other_messages(update: Update, context: CallbackContext) -> None:
    pass

def main() -> None:
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

    # Register a message handler for all other text messages in the group chat
    dispatcher.add_handler(MessageHandler(Filters.text & ~Filters.command, handle_other_messages))

    # Start listening for updates
    updater.start_polling()
    updater.idle()

if __name__ == '__main__':
    main()
