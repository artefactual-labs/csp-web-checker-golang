# CSP Web Checker (Go + Playwright)

A small Go web service that runs CSP checks using a real browser engine (Playwright) and stores profiles + run history in SQLite.

This project exists to avoid confusion when using a regular browser with DevTools: console output can mix messages from extensions, other tabs, or background pages, and CSP diagnostics are not always easy to filter or configure correctly. This service isolates each URL check and captures only CSP violations for that page.

## Requirements

- Go 1.21+ (for building from source)
- Node.js 18+ (runtime for Playwright)

## Install Playwright (source builds)

From the project root:

```bash
npm init -y
npm i playwright
npx playwright install chromium
```

If you already have a `package.json`, you can skip `npm init -y`.

## Build and Run (source)

```bash
go mod tidy
go build -o csp-web
./csp-web
```

Or run directly:

```bash
go run .
```

Open `http://127.0.0.1:8080`.

## Install from Packages (deb/rpm)

### 1) Install the package

Debian/Ubuntu:

```bash
sudo dpkg -i csp-web-checker-golang_<version>_amd64.deb
```

RHEL/Rocky/Alma/Fedora:

```bash
sudo rpm -Uvh csp-web-checker-golang_<version>_x86_64.rpm
```

This creates the system user `csp-check`, installs the service unit, and writes defaults to `/etc/default/csp-web`.

### 2) Install Node.js

Install Node.js 18+ from your distro or NodeSource. Example (Debian/Ubuntu):

```bash
curl -fsSL https://deb.nodesource.com/setup_18.x | sudo -E bash -
sudo apt-get install -y nodejs
```

If you prefer a tarball install (no apt), you can use:

```bash
sudo mkdir -p /opt/node
cd /tmp
NODE_VERSION=20.19.2
curl -fsSL -o node.tar.xz "https://nodejs.org/dist/v${NODE_VERSION}/node-v${NODE_VERSION}-linux-x64.tar.xz"
sudo tar -xJf node.tar.xz -C /opt/node --strip-components=1
sudo ln -sf /opt/node/bin/node /usr/local/bin/node
sudo ln -sf /opt/node/bin/npm /usr/local/bin/npm
sudo ln -sf /opt/node/bin/npx /usr/local/bin/npx
```

### 3) Install Playwright dependencies

Playwright needs extra system libraries for Chromium. Examples:

Debian/Ubuntu:

```bash
sudo apt-get install -y \
  libnss3 libnspr4 libatk1.0-0 libatk-bridge2.0-0 libcups2 \
  libdrm2 libdbus-1-3 libxkbcommon0 libxcomposite1 libxdamage1 \
  libxfixes3 libxrandr2 libgbm1 libasound2 libpango-1.0-0 \
  libpangocairo-1.0-0 libcairo2 libx11-xcb1 libx11-6 libxext6 \
  libxrender1 libxtst6 libxshmfence1 libxss1 ca-certificates
```

RHEL/Rocky/Alma/Fedora:

```bash
sudo dnf install -y \
  nss nspr atk at-spi2-atk cups-libs libdrm dbus-libs libxkbcommon \
  libXcomposite libXdamage libXfixes libXrandr mesa-libgbm alsa-lib \
  pango cairo libX11 libXext libXrender libXtst libXshmfence libXss ca-certificates
```

Ubuntu 20.04 missing libraries (after Chromium/GTK deps):

```bash
sudo apt-get install -y \
  libatomic1 libopus0 libwebpdemux2 libharfbuzz-icu0 \
  libwebpmux3 libenchant-2-2 libsecret-1-0 libhyphen0 \
  libgbm1 libegl1 libglx0 libevdev2 libgles2 libx264-155 libwoff1
```

If you plan to use Firefox or WebKit, install dependencies for those browsers too (recommended, as the exact package list changes over time):

```bash
sudo npx playwright install-deps firefox
sudo npx playwright install-deps webkit
```

### 4) Install Playwright for the service user

Playwright downloads browser binaries per user. Install them for `csp-check`:

```bash
sudo mkdir -p /var/lib/csp-web/.npm-global /var/lib/csp-web/.npm-cache
sudo chown -R csp-check:csp-check /var/lib/csp-web
sudo -u csp-check -H bash -c "export HOME=/var/lib/csp-web; npm config set prefix /var/lib/csp-web/.npm-global; npm config set cache /var/lib/csp-web/.npm-cache; /usr/local/bin/npm install -g playwright"
sudo -u csp-check -H bash -c "export HOME=/var/lib/csp-web; /var/lib/csp-web/.npm-global/bin/playwright install chromium firefox webkit"
```

If `npm` is not found in the service user PATH, ensure Node.js is installed system-wide or update `/etc/default/csp-web` to point to the correct `CSP_NODE_BIN`.

### 5) Configure defaults

Edit `/etc/default/csp-web` to set the listener address, DB location, and Node/Playwright paths.

```bash
sudo editor /etc/default/csp-web
```

Make sure the PATH includes the Playwright global bin for `csp-check`:

```
PATH=/var/lib/csp-web/.npm-global/bin:/usr/local/bin:/usr/bin:/bin
NODE_PATH=/var/lib/csp-web/.npm-global/lib/node_modules
```

Then restart the service:

```bash
sudo systemctl restart csp-web
```

## Updating Playwright Browsers

Playwright bundles browser versions and should be kept up to date. When you update Playwright, re-run the install command to refresh browser binaries.

For the `csp-check` user:

```bash
sudo -u csp-check -H bash -c "export HOME=/var/lib/csp-web; /usr/local/bin/npm install -g playwright@latest"
sudo -u csp-check -H bash -c "export HOME=/var/lib/csp-web; /var/lib/csp-web/.npm-global/bin/playwright install"
```

To update browsers and system dependencies in one step:

```bash
sudo -u csp-check -H bash -c "export HOME=/var/lib/csp-web; /var/lib/csp-web/.npm-global/bin/playwright install --with-deps"
```

## First Run (Quick Example)

1) Start the server:

```bash
go run .
```

2) Open `http://127.0.0.1:8080` and paste:

```
https://breckhistoryarchives.org/
https://breckhistoryarchives.org/robots.txt
https://breckhistoryarchives.org/favicon.ico
```

3) Submit, then open **Run History** to view the results.

## Usage

- Paste full URLs (one per line) on the home page and submit.
- View results in Run History and click a run for details.
- Grouped Issues summarize violations across pages; Page Status shows HTTP status and timings for every URL.
- Each run checks Chromium, Firefox, and WebKit sequentially and shows three sections in the results.

## Configuration

Environment variables:

- `CSP_WEB_ADDR` (default `127.0.0.1:8080`)
- `CSP_WEB_DB` (default `data.db` or `/var/lib/csp-web/data.db` for packages)
- `CSP_NODE_BIN` (default `node`)
- `CSP_SCRIPT_PATH` (default `./csp-check.mjs` or `/usr/local/bin/csp-check.mjs` for packages)

## Notes

- Lines starting with `#` in the URL list are ignored.
- Only full `http://` or `https://` URLs are accepted.

## Resetting the Database

The app stores profiles and run history in a SQLite file. To reset it:

Source runs (local file):

```bash
rm -f data.db
```

Package install (systemd):

```bash
sudo systemctl stop csp-web
sudo rm -f /var/lib/csp-web/data.db
sudo systemctl start csp-web
```

On next start, the app recreates the database and default profiles.
