# Setup Guide (for everyone)

This guide helps you run a small **XDC blockchain on your own computer** — even
if you've never touched blockchain or coding before. Just follow the steps.

---

## What is this, in plain words?

Think of it as a **tiny private version of the XDC network running only on your
machine**. It starts **4 "computers" (called nodes)** that talk to each other
and agree on a shared record of blocks — exactly like the real network, but
private, free, and fully under your control. Great for learning and testing.

Nothing here touches the real XDC network or real money.

---

## Before you start

Works on **Mac** and **Linux / Ubuntu**. The blockchain programs are built for
you automatically on the first run, so you install a few free developer tools
once.

**Mac:** open **Terminal** (Spotlight: `Cmd + Space`, type "Terminal"), then:

```
xcode-select --install      # developer tools
brew install go             # the Go language (or download from https://go.dev/dl)
```

**Linux / Ubuntu:**

```
sudo apt update && sudo apt install -y build-essential git
# Go 1.23+ is required; Ubuntu's 'golang' package is usually too old, so:
wget https://go.dev/dl/go1.23.4.linux-amd64.tar.gz
sudo rm -rf /usr/local/go && sudo tar -C /usr/local -xzf go1.23.4.linux-amd64.tar.gz
export PATH=$PATH:/usr/local/go/bin   # add this line to ~/.profile to persist
```

The first run downloads a ready-built node binary from the public mirror and
checks it against a recorded sha256 (under a minute, one time). **You do not
need a GitHub account or any org access** — this includes `oldxdc`, the
default client. Nothing on this path uses `git clone` or the GitHub CLI.

`gh` is only required if you deliberately force a build from source
(`BIN_SOURCE=build`), because the client *source* repos are private. You do
not need to do that to follow this guide. After the first run, everything
below is the same on both.

---

## Start it — one command

1. Open **Terminal**.
2. Go into this project folder. Type `cd ` (with a space), then drag the folder
   onto the Terminal window and press **Enter**. For example:

   ```
   cd /Users/you/XDCNetwork/Local_DPoS_Setup
   ```

3. Run this single command and press **Enter**:

   ```
   ./setup.sh --new
   ```

That's it. The **first run builds the software (a few minutes, one time)**;
after that it starts in about **20–30 seconds**.

### What that one command does (so it's not a mystery)

1. **Creates 4 new keys** — like 4 unique ID cards for the 4 nodes.
2. **Builds the rulebook** (the "genesis") that says who's allowed to create blocks.
3. **Clears any old data** so you start clean.
4. **Starts everything** — the 4 nodes plus a small "phone book" service
   (the *bootnode*) that helps them find each other, plus a 5th node running
   the modern go-ethereum client that just listens in and copies the same
   blocks (it never creates any) — a quick way to see a different program
   agreeing with the 4 nodes above.

---

## How do I know it's working?

Once it's running, the Terminal will keep printing status lines — that's normal,
it means the nodes are alive and creating blocks. Leave that window open.

To check the current block count, open a **second** Terminal window, `cd` into
the folder again, and run:

```
curl -s -X POST -H 'Content-Type: application/json' \
  --data '{"jsonrpc":"2.0","method":"eth_blockNumber","params":[],"id":1}' \
  http://localhost:8545
```

You'll see something like `{"...":"0x1f"}`. The number after `0x` grows over
time — that means new blocks keep being added. **A rising number = it's healthy.**
(A new block appears roughly every 2 seconds.)

---

## Review what your network looks like

Want a plain summary of your network — who creates blocks, who owns what, the
starting balances? Run:

```
./network-info.sh
```

It prints an easy-to-read report. The masternodes marked "YOU control this" are
the ones whose keys are in your `.env`. Accounts without a "[yours]" mark are
placeholders you don't hold keys for — normal for a local test network.

## How do I stop it?

Click the Terminal window that's printing all the status lines and press:

```
Ctrl + C
```

Everything shuts down cleanly.

---

## How do I start over from scratch?

Run these two lines:

```
./reset.sh
./setup.sh --new
```

`reset.sh` erases the old blockchain data, and `setup.sh --new` builds a fresh
one with brand-new keys.

To restart with the **same** keys you already have (not new ones), just run:

```
./setup.sh
```

---

## Connecting a second computer (optional)

Want another machine on your network to join? On the computer that's already
running, get the "join address" (the *bootnode endpoint*):

```
echo "enode://$(./bin/*/bootnode -nodekey ./bootnode.key -writeaddress)@$(ipconfig getifaddr en0):30301"
```

Copy the line it prints and give it to whoever runs the second machine — the
full instructions for the second machine are in `README.md` under
"Connecting from another machine". Both computers must be on the same network.

---

## Tiny glossary

| Word | Plain meaning |
|------|---------------|
| **Node** | One computer/program participating in the blockchain. We run 4. |
| **Bootnode** | A "phone book" that helps nodes find each other. |
| **Block** | One page in the shared record book; a new one is added every ~2s. |
| **Genesis** | The very first page + the rules of the network. |
| **Key / address** | A node's private password and its public ID. |
| **Masternode** | A node allowed to create new blocks. |

---

## If something goes wrong

- **"Permission denied"** → run `chmod +x *.sh` once, then try again.
- **"address already in use"** → an old copy is still running. Close it with
  `Ctrl + C`. If a node from an earlier run is still up, find it by its PORT
  rather than by name — `ss -ltnp "sport = :8545"`, plus `ss -lunp` for UDP
  since the bootnode's discovery port is UDP-only — then `kill -TERM <pid>`.
  Avoid `pkill -f`: it matches on a substring and can kill unrelated
  processes (including your own shell) or other people's nodes on a shared
  machine.
- **The block number isn't growing** → stop (`Ctrl + C`), then
  `./reset.sh` and `./setup.sh --new`.
- **Still stuck?** Share the last ~20 lines from the Terminal with whoever set
  this up for you.

---

## Note on the "join address" command

The command above uses `ipconfig getifaddr en0`, which is for Mac. On Linux use:

```
echo "enode://$(./bin/*/bootnode -nodekey ./bootnode.key -writeaddress)@$(hostname -I | awk '{print $1}'):30301"
```

## Windows note

Windows isn't directly supported. Use **WSL2** (Ubuntu inside Windows) and then
follow the Linux instructions above.
