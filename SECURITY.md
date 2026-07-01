# Security Policy

## Supported versions

This project is currently pre-1.0. Security fixes are applied to `main`.

## Reporting a vulnerability

Please do not open a public issue for sensitive vulnerabilities. Report privately via GitHub Security Advisories when available, or contact the repository owner.

## Security model

`local-fusion-gateway` is a local OpenAI-compatible fusion gateway. It can forward prompts to configured upstream providers. Treat every configured provider as able to read the prompts and model outputs sent to it.

Production recommendations:

- Set `auth_token_env` and require `Authorization: Bearer ...` for `/v1/*` endpoints.
- Bind to `127.0.0.1` unless you put the service behind a trusted reverse proxy with TLS.
- Keep API keys in environment variables, not config files.
- Keep `debug.capture_content: false` unless you explicitly accept storing prompt/response content.
- Review `code_research.workdir` carefully; code research tools are read-only but intentionally expose selected source files to upstream models.
