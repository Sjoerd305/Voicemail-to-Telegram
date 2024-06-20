#!bin/bash
exec python3 main.py &
exec python3 telegram_listener.py &
exec python3 emailcleanup.py