# Secrets Format for Nenya Gateway

Nenya uses systemd credentials to securely load API keys and tokens. The secrets file must be a JSON file with the following structure:

```json
{
  "client_token": "your-client-bearer-token-here",
  "gemini_key": "gemini-api-key-here",
  "deepseek_key": "deepseek-api-key-here",
  "zai_key": "zai-api-key-here"
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

## Upstream API Keys

- `gemini_key`: Google Gemini API key
- `deepseek_key`: DeepSeek API key  
- `zai_key`: z.ai API key

At least one upstream key must be present for the corresponding provider.

## Security Notes

- Never commit secrets to version control
- The secrets file should be readable only by the service user (e.g., `nenya:nenya`)
- Use systemd's credential mechanism for secure in-memory storage
- Rotate tokens periodically