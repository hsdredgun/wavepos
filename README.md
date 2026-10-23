# WavePos

A tiny point-of-sale for taking Bitcoin payments at a shop counter, built on
[Wavelength](https://wavelength.lightning.engineering/), Lightning Labs' Ark
wallet.

Punch in an amount in your local currency, the customer scans a Lightning QR,
and the screen goes green when it's paid. There are no Lightning channels to
open, no inbound liquidity to manage, and no domain to buy.

<img src="docs/keypad.jpg" alt="WavePos keypad" width="420">

> [!WARNING]
> **Beta software on top of beta software.** Wavelength mainnet is
> invite-only and still changing. Treat this as a "we also accept Bitcoin"
> option, sweep often, and don't keep more on it than you can afford to lose.

## How it works

```
Tablet browser ──HTTPS──> wavepos (Go, one binary) ──REST──> waved (Wavelength daemon)
                                                                    │
       wave-sweep.timer: balance over threshold → sweep to on-chain cold storage
```

- **wavepos** serves the keypad page, converts fiat to sats (Coinbase, with
  Kraken as fallback), asks `waved` for a Lightning invoice and polls it until
  it's paid.
- Customers pay with **any Lightning wallet**. Payers who also use Wavelength
  settle directly inside Ark.
- It runs with a **receive-only macaroon**: it can create invoices and read
  their status, but it cannot spend.
- **wave-sweep** moves the balance to an on-chain address when it gets big,
  because a Wavelength wallet can only hold so much (see limits).

## Operator limits (as of Wavelength v0.1.99)

Check yours with `wavecli getinfo` → `server_info`.

| Limit | Value | What WavePos does about it |
|---|---|---|
| `max_vtxo_amount` | 50,000 sats | Bills above this are split into several equal QR payments, each 10% under the limit. The customer sees a short note explaining why. |
| `max_user_balance` | 300,000 sats | Before each invoice WavePos checks the balance. If the payment would go over, the screen asks staff to wait for the sweep. |
| `min_vtxo_amount_sat` | 1,000 sats | Smaller payments still work: they land as server credit and turn into a VTXO once credit passes 1,000. |

When the operator raises the limits, change `-max-invoice-sats` and
`-max-balance-sats`.

## Setup

These steps assume Linux with systemd, Go 1.22+ and `jq`.

### 1. Run Wavelength on mainnet

Build `waved` and `wavecli` from
[lightninglabs/wavelength](https://github.com/lightninglabs/wavelength)
(`make install-wavewalletrpc`). Run `waved` with `--network=mainnet
--allow-mainnet --wallet.type=lwwallet`, create a wallet with `wavecli create`,
and write the seed down on paper.

Mainnet is allowlisted. Run `wavecli getinfo` and submit your
`identity_pubkey` through the access form linked from the
[Networks & config](https://wavelength.lightning.engineering/concepts/networks-and-config/)
docs.

`waved` must stay running. It refreshes your VTXOs for free before their
batch expires (in the operator's free refresh window). If it's offline for
that whole window, the operator may sweep the expired batch.

### 2. Bake a receive-only macaroon

```sh
wavecli bakemacaroon \
  uri:/wavewalletrpc.WalletService/Recv \
  uri:/wavewalletrpc.WalletService/Status \
  uri:/wavewalletrpc.WalletInspectionService/InspectActivity \
  --save-to /root/.waved/pos.macaroon
chmod 600 /root/.waved/pos.macaroon
```

### 3. Build and install

```sh
git clone https://github.com/hsdredgun/wavepos && cd wavepos
go build -o wavepos .
install -m 755 wavepos /usr/local/bin/wavepos
```

### 4. Shop key (optional, recommended)

If the POS is reachable from the internet, lock it with a shared key:

```sh
(umask 077; openssl rand -hex 16 > /root/.wavepos-key)
```

Open `https://<your-pos>/?k=<key>` once on the till and add it to the home
screen. Staff just tap the icon. Anyone without the key sees `Locked`.

### 5. Run it with systemd

```sh
cp contrib/wavepos.service /etc/systemd/system/
systemctl daemon-reload && systemctl enable --now wavepos
```

Flags:

| Flag | Default | |
|---|---|---|
| `-listen` | `127.0.0.1:8080` | Use `:8080` to also allow LAN access |
| `-macaroon` | (required) | The receive-only macaroon |
| `-currency` | `AUD` | Any code Coinbase/Kraken quote against BTC (USD, EUR, GBP…) |
| `-memo` | empty | Prefix for the invoice memo. Payers can read it, so leave it empty if you'd rather not name your shop |
| `-key-file` | empty | Shop key file; empty leaves the POS unlocked |
| `-max-invoice-sats` | `50000` | Operator `max_vtxo_amount` |
| `-max-balance-sats` | `300000` | Operator `max_user_balance` |
| `-waved` | `http://127.0.0.1:10031` | `waved` REST gateway |

### 6. Put it online with Tailscale Funnel

[Funnel](https://tailscale.com/kb/1223/funnel) gives you a public
`https://<machine>.<tailnet>.ts.net` address with a real certificate. You
don't need a domain or port forwarding, and the till doesn't need Tailscale
installed.

```sh
tailscale up --hostname=wavepos
tailscale funnel --bg 8080
```

A new Funnel name can take up to half an hour to show up in public DNS.

If this runs in a **Proxmox LXC container**, give it a TUN device first by
adding this to `/etc/pve/lxc/<id>.conf` on the host, then rebooting the
container:

```
lxc.cgroup2.devices.allow: c 10:200 rwm
lxc.mount.entry: /dev/net/tun dev/net/tun none bind,create=file
```

### 7. Auto-sweep to cold storage

Edit `ADDR` (and the thresholds, if you like) in `contrib/wave-sweep.sh`,
then run:

```sh
install -m 755 contrib/wave-sweep.sh /usr/local/bin/wave-sweep.sh
cp contrib/wave-sweep.service contrib/wave-sweep.timer /etc/systemd/system/
systemctl daemon-reload && systemctl enable --now wave-sweep.timer
```

Every 2 minutes it checks the confirmed balance. From 150k sats it previews a
cooperative exit and sweeps everything if the fee is under 2,000 ppm. From
250k sats it accepts up to 10,000 ppm, so the wallet never fills up. The
operator sets the exit fee; a single-VTXO exit cost about 285 sats in
testing.

The sweep uses the admin macaroon and runs separately from the POS, so the
web-facing process never holds spending rights.

## License

MIT
