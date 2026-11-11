## Logging & Observability — Mandatory Development Rules

**These rules are mandatory for all new code, bug fixes, refactoring, and architectural changes.**

Logging and observability are core architectural requirements, not optional enhancements. Every change MUST preserve or improve the project's existing observability standards.

### 1. Mandatory Instrumentation

- Every new feature MUST include appropriate logging for significant operations, state transitions, failures, and outcomes.
- Every modified feature MUST preserve or improve its existing logging.
- Every new integration, background task, scheduled job, or external operation MUST provide sufficient diagnostic information.
- Application startup, shutdown, initialization failures, and critical lifecycle events MUST be observable.
- Significant operations SHOULD include execution duration and outcome when useful.
- Logging MUST be proportional to operational relevance. Do not log every function call unnecessarily.

### 2. Logging Architecture

- ALWAYS use the project's established logging abstraction, configuration, and conventions.
- NEVER introduce independent logging implementations without architectural justification.
- NEVER use `print`, `console.log`, `console.error`, or equivalent statements for production diagnostics when an established logger exists.
- CLI applications MUST keep functional stdout output separate from diagnostic logging.
- Prefer structured logging and stable, machine-readable event names.
- Logging MUST remain decoupled from business logic.
- Logging MUST NOT introduce unnecessary dependencies or infrastructure.

### 3. Logging Levels

Use logging levels consistently:

- TRACE: highly detailed execution diagnostics.
- DEBUG: technical information useful during debugging.
- INFO: significant successful operations and lifecycle events.
- WARN: unexpected but recoverable conditions.
- ERROR: failed operations requiring investigation.
- FATAL/CRITICAL: failures preventing continued operation, where supported.

NEVER use ERROR for expected business outcomes.

NEVER use INFO or ERROR for excessive routine diagnostic output.

### 4. Structured Context & Correlation

Relevant log events SHOULD include, where applicable:

- Timestamp (UTC).
- Severity level.
- Component or module.
- Stable event name.
- Operation name.
- Outcome.
- Duration.
- Correlation/request ID.
- Trace/span ID when tracing is enabled.
- Error type and code.

- Preserve correlation context across asynchronous boundaries when applicable.
- NEVER generate fake trace or span identifiers.
- Prefer consistent structured fields over string concatenation.
- Avoid high-cardinality or unnecessary contextual data.

### 5. Security & Privacy

- NEVER log passwords, credentials, tokens, API keys, session cookies, private keys, or other secrets.
- NEVER log unnecessary personal, confidential, or sensitive information.
- NEVER log complete request/response bodies by default.
- Apply redaction and data minimization before logging.
- Treat untrusted log data as potentially malicious.
- Prevent log injection and unsafe serialization.
- Stack traces MUST NOT expose sensitive data.
- Security-relevant actions MUST be auditable when required by the project's threat model.
- Security audit trails MUST NOT be treated as interchangeable with diagnostic logs.

### 6. Error Handling

- NEVER silently swallow unexpected exceptions.
- Log errors at the appropriate architectural boundary.
- Include actionable diagnostic context without exposing secrets.
- Preserve original error causes.
- Avoid logging the same exception repeatedly across layers.
- Expected errors MUST be handled without unnecessary error-level logging.
- Unhandled failures MUST be observable at the appropriate application boundary.

### 7. Performance & Reliability

- Logging MUST NOT significantly degrade application performance.
- Avoid expensive serialization when the configured log level is disabled.
- Avoid excessive logging inside hot paths, loops, and high-frequency events.
- Use sampling, rate limiting, or aggregation where appropriate.
- Logging failures MUST NOT crash the application unless explicitly required by audit or compliance guarantees.
- Respect the project's existing rotation, retention, buffering, and output policies.
- Offline-first applications MUST NOT depend on external logging services.

### 8. Testing Requirements

Whenever a change introduces or modifies observable behavior:

- Add or update tests for relevant log events.
- Verify correct severity and structured fields.
- Verify sensitive information is not exposed.
- Verify correlation propagation when applicable.
- Verify error paths remain observable.
- Ensure logging changes do not alter business behavior.

Tests SHOULD validate structured event properties rather than brittle human-readable message strings.

### 9. Definition of Done

**A development task MUST NOT be considered complete until:**

1. Relevant new operations are appropriately instrumented.
2. Existing observability has not regressed.
3. Errors and important outcomes can be diagnosed.
4. Logs follow established naming, severity, and structural conventions.
5. No sensitive information is exposed.
6. Relevant logging tests pass.
7. No unnecessary logging noise or performance regressions are introduced.
8. Logging documentation is updated when conventions or architecture change.

Before completing any development task, explicitly review its logging and observability impact.

If no additional logging is necessary, preserve the existing architecture and do not add artificial events.

**Golden Rule: Every significant operation must be diagnosable without modifying the source code or reproducing the issue with additional print statements.**
