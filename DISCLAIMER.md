# Disclaimer

**Nenya AI Gateway** ("Nenya") is provided "as is" for educational and operational use. By using this software, you acknowledge and agree to the following terms.

## 1. Best-Effort Security — Not a Guarantee

Nenya includes a regex-based security interceptor designed to detect and block potentially destructive CLI commands (e.g., `rm -rf`, `terraform destroy`, `DROP TABLE`) before they reach your terminal or infrastructure.

**This is a best-effort defense layer, not a foolproof guarantee.**

Large language models are capable of generating obfuscated, aliased, or otherwise non-obvious command variants that may bypass regex patterns. Examples include but are not limited to:

- Shell aliases and function redefinitions
- Encoded or base64-wrapped commands
- Indirect execution via interpreters (`python -c`, `eval`, `sh -c`)
- Chained commands with non-standard delimiters

**No automated filter can fully prevent a determined or hallucinating LLM from producing harmful output.** You are solely responsible for validating any command before execution.

## 2. Autonomous Agent Risk — Zero Liability

Granting autonomous AI agents access to a terminal, cloud infrastructure, or production systems is an inherently dangerous operation with real-world consequences.

**Rafael Gumieri, the Nenya contributors, and all affiliated parties are strictly NOT liable** for any damage, loss, or harm arising from the use of this software, including but not limited to:

- Data loss or corruption
- Infrastructure demolition or misconfiguration
- Unauthorized cloud billing or resource consumption
- System crashes, denial of service, or availability incidents
- Security breaches caused by commands routed through Nenya
- Any financial, legal, or reputational consequences

**Use at your own risk.**

## 3. Human-in-the-Loop Recommendation

We strongly recommend using Nenya in conjunction with **human oversight**, particularly when agents interact with:

- **Production infrastructure** — `kubectl`, `helm`, `terraform`, `aws`, `gcloud`, `az`
- **Destructive operations** — database migrations, certificate rotations, DNS changes
- **Billing-sensitive services** — auto-scaling groups, spot instances, cross-region replication
- **Shared or multi-tenant environments** — where blast radius extends beyond your personal workspace

Nenya is designed to **reduce risk**, not eliminate it. The safest deployment is one where a human operator reviews and approves agent-generated commands before they execute.

## 4. AI-Assisted Development

This project was rapidly prototyped and built in collaboration with AI engineering tools. We proudly embrace the modern era of AI-assisted software development — the same paradigm that Nenya itself is designed to make safer.

Every line of code has been reviewed, tested, and validated by the maintainer. AI tools accelerated the development process; they did not replace human engineering judgment, architectural decisions, or accountability for the final product.

---

*Nenya is licensed under the Apache License 2.0. See the [LICENSE](LICENSE) file for full legal terms.*
