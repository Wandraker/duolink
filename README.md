# Duolink

Use several internet connections on one Windows PC at the same time: router cable, phone over USB, Wi-Fi, a second cable. Everything runs locally, no server or account needed.

[Русский ниже](#русский)

## What it does

- **Finds your connections.** Every interface that reaches the internet becomes a channel. Plug a phone in and it shows up within 10 seconds; unplug it and traffic moves to the rest.
- **Multi-channel downloads.** Paste a link and the file is split into pieces fetched over all channels at once. A single file gets close to the sum of your connections. Interrupted downloads resume.
- **SOCKS5 proxy for other apps.** qBittorrent, browsers, Telegram and anything else with SOCKS5 support spread their connections across channels. Torrents gain the most because they open many connections.
- **Sticky sites.** The same site stays on the same channel for 10 minutes, so logins and banking that check your IP keep working.
- **Live panel** at `http://127.0.0.1:7878` with per-channel speed, ping and share.

## Limits

One connection always runs over one channel. A single live stream (a game, a call, one TCP download in another app) is not faster. Splitting one connection across links needs a server on the other end; that is not what Duolink does.

## Run

```
duolink.exe
```

Options: `-ui 127.0.0.1:7878`, `-socks 127.0.0.1:1080`, `-dir <folder>`, `-workers 4`, `-no-browser`.

Command line download:

```
duolink.exe get -o D:\Downloads https://example.com/big.iso
```

## Build

Go 1.22 or newer:

```
go build -o duolink.exe ./cmd/duolink
```

## License

GPL-3.0, see [LICENSE](LICENSE). Contributions require the [CLA](CLA.md).

---

## Русский

Duolink позволяет пользоваться сразу несколькими подключениями к интернету на одном ПК с Windows: кабель от роутера, телефон по USB, Wi-Fi, второй кабель. Всё работает локально, без сервера и аккаунта.

- **Сам находит подключения.** Подключили телефон — через 10 секунд он появится как канал. Отключили — трафик уйдёт на остальные.
- **Загрузка через все каналы.** Вставьте ссылку: файл делится на куски и качается сразу через все каналы. Прерванная загрузка продолжится с того же места.
- **SOCKS5-прокси для других программ.** qBittorrent, браузер, Telegram раскидывают соединения по каналам. Торренты ускоряются сильнее всего.
- **Липкие сайты.** Один сайт 10 минут держится на одном канале, поэтому входы в аккаунты и банки не ломаются.
- **Панель** `http://127.0.0.1:7878` со скоростью, пингом и долей каждого канала.

Ограничение: одно соединение всегда идёт через один канал. Игра, звонок или одиночная загрузка в другой программе быстрее не станут.

Лицензия GPL-3.0. Для правок в проект нужно принять [CLA](CLA.md).
