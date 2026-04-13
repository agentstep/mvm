# Security Policy

## Reporting a Vulnerability

If you discover a security vulnerability, please report it responsibly. **Do not open a public GitHub issue.**

Email **security@agentstep.com** with:

- Description of the vulnerability
- Steps to reproduce
- Affected component (CLI, agent, VM networking, Lima integration)
- Severity estimate (critical, high, medium, low)

## Response Timeline

- **Acknowledge**: within 48 hours
- **Triage**: within 1 week
- **Fix**: depends on severity, critical issues are prioritized

## Scope

In scope:
- VM escape or isolation bypass
- Network sandbox bypass
- Agent protocol vulnerabilities
- Command injection via exec/SSH
- Privilege escalation within or between VMs

Out of scope:
- Issues requiring physical access to the host
- Vulnerabilities in upstream dependencies (Firecracker, Lima) — report those to the respective maintainers
- Denial of service via resource exhaustion on localhost

## Disclosure

We will coordinate disclosure with you and credit you in the release notes unless you prefer anonymity.
