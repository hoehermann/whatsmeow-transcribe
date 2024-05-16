# whatsmeow-transcribe

This is a small service app for transcribing (speech-to-text) WhatsApp voice messages. It is powered by [whatsmeow](https://github.com/tulir/whatsmeow/) and the [openai/whisper API](https://platform.openai.com/docs/guides/speech-to-text).

1. Clone the repository.
2. Run `go build` inside this directory.
3. Run `./whatsmeow-transcribe --api-key sk-proj-YOUR-API-KEY-HERE` to start the program.
4. On the first run, scan the QR code. On future runs, the program will remember you (unless `whatsmeow.db` is deleted). 

Any voice message sent to your account will be transcribed. The speech-to-text result is automatically posted to the conversation *for everyone to see*.

![Screenshot](/screenshot.png?raw=true "Screenshot")

You can also use the `API_KEY` environment variable to supply the API key.  
In case you are running a local text-to-speech instance, you can have `--api-url` point to your server. 

This is a proof of concept. No support is provided.
