# teleglogger

Watches docker logs for regexp and send a Telegram message when matching. Also sends a message when container dies.

It's something like [logspout](https://github.com/gliderlabs/logspout), but simpler, only with one adapter/route - a Telegram bot chat. I dug most of the code from lospout actually.

## Usage

Use `./rebuild.sh` to rebuild the project into image `tomk/teleglogger`.

See `docker-compose.yml` for usage with containers.

## Prerequisities

Before you can use this, you need to create a Telegram bot and get a token (sth like `5747861481:AAfsdfFfgdfgdFGDFGdfgDFGdfg`). Then, you need to send a message to the bot and find out chat ID of the conversation. 

You can find the chat ID by HTTP request to the Bot API, 

```
$ curl https://api.telegram.org/5747861481:AAfsdfFfgdfgdFGDFGdfgDFGdfg/getUpdates
```

Look for `result[0][message][from][id]` in the response JSON.

```json
{
  "ok": true,
  "result": [
    {
      "update_id": 542980616,
      "message": {
        "message_id": 4,
        "from": {
          "id": 568975557, <<<<< This

```


## Environment variables

Teleglogger is configured with envvars, you can see it from the code, but the most important are:

- `TG` - if unset or "0", it will not send Telegram messages. I.e. you must set it to for example "1" if you want to have Telegram messages sent.
- `TG_CHAT` - Telegram ChatID 
- `TG_TOKEN` - Telegram Bot token
- `MATCHRE` - Golang regexp for matching message that you want to be sent to Telegram
- `DEBUG` - Set to "1" for more verbosity




 
