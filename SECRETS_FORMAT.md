# Secrets Format for Nenya Gateway

Nenya uses systemd credentials to securely load API keys and tokens. The secrets file must be a JSON file with the following structure:

```json
{
  "client_token": "your-client-bearer-token-here",
  "provider_keys": {
    "gemini": "AIza...",
    "deepseek": "sk-...",
    "zai": "...",
    "groq": "gsk_...",
    "together": "...",
    "openai": "sk-..."
  }
}
```

## Deployment with systemd

1. Create a JSON file with the above format (e.g., `/etc/nenya/secrets.json`)
2. Configure the systemd service unit to load it as a credential:

```ini
[Service]
...
LoadCredential=secrets:/etc/nenya/secrets.json
```

The gateway will read the file from `$CREDENTIALS_DIRECTORY/secrets`.

## Generating a Client Token

The `client_token` is used by AI clients (OpenCode/Aider) to authenticate to Nenya. Generate a secure random token:

```bash
openssl rand -hex 32
```

## Provider Keys

The `provider_keys` object maps provider names (matching provider keys in `config.json`) to their API keys. Built-in provider names are: `gemini`, `deepseek`, `zai`, `groq`, `together`, `ollama`.

To add a custom provider (e.g., OpenAI), add its key under the matching provider name defined in `config.json`:

```json
{
  "provider_keys": {
    "openai": "sk-proj-..."
  }
}
```

At least one provider key must be present for the corresponding provider to work.

## Security Notes

- Never commit secrets to version control
- The secrets file should be readable only by the service user (e.g., `nenya:nenya`)
- Use systemd's credential mechanism for secure in-memory storage
- Rotate tokens periodically
