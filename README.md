# CLIProxyAPI Usage Quota Statistics

[简体中文](README_CN.md) | English

A native plugin for [CLIProxyAPI](https://github.com/router-for-me/CLIProxyAPI) that tracks token usage and estimated cost per credential configuration and model. It adds a dedicated **Usage Quota Statistics** page to the management panel sidebar.

## Features

- Aggregates successful requests by credential (`AuthID`) and model.
- Tracks regular input, output, cache-read, and cache-write tokens separately.
- Calculates estimated cost from user-defined prices per one million tokens.
- Supports exact model names and prefix patterns such as `claude-*`.
- Calculates cache hit rate as `cache read / (regular input + cache read + cache write)`.
- Handles the different cache-token accounting conventions used by OpenAI-compatible and Claude/Anthropic providers.
- Persists usage locally as JSON Lines, so statistics survive server restarts.
- Clearly marks models that do not have a configured price instead of treating them as free.

## Install from the plugin store

Add this registry to `config.yaml`:

```yaml
plugins:
  enabled: true
  dir: "plugins"
  store-sources:
    - "https://raw.githubusercontent.com/HunterWangwei/usage-quota-stats/main/registry.json"
  configs:
    usage-quota-stats:
      enabled: true
```

Restart CLIProxyAPI, open the plugin store, install **Usage Quota Statistics**, and restart the server once more to load the native library.

## Configure model prices

Prices use the configured currency per one million tokens. Replace the example zeroes with your actual prices:

```yaml
plugins:
  configs:
    usage-quota-stats:
      enabled: true
      currency: USD
      data-file: "usage-quota-stats.jsonl"
      prices:
        gpt-5.4:
          input: 0
          output: 0
          cache-read: 0
          cache-write: 0
        claude-*:
          input: 0
          output: 0
          cache-read: 0
          cache-write: 0
```

Exact model names take precedence over patterns. When several patterns match, the longest prefix wins. Models without a matching price remain visible and are marked as unpriced.

Relative `data-file` paths are resolved from the CLIProxyAPI working directory. For Docker deployments, persist both the plugin and usage-data paths:

```yaml
volumes:
  - ./plugins:/app/plugins
  - ./data:/app/data
```

Then set `data-file: /app/data/usage-quota-stats.jsonl`.

## Build from source

Requirements:

- Go 1.26 or newer
- CGO enabled
- A C compiler supported by the target platform

Windows PowerShell:

```powershell
./build.ps1
```

Linux:

```sh
chmod +x build.sh
./build.sh
```

The output is written under `dist/<goos>/<goarch>/`.

## Data and privacy

The plugin stores credential identifiers, model names, timestamps, and token counters. It does not store prompts, responses, API keys, or OAuth access tokens. The JSONL data file is created with owner-only permissions where supported.

## License

[MIT](LICENSE)
