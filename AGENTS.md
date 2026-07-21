# Repository Guidelines

- Run `just test` after changes; it formats, diagnoses, vets, and race-tests the project, including the real-restic test when available.
- Keep implementation and tests together. For web or download changes, cover status, headers, escaping, path encoding, and streaming.
- Inline straightforward code and extract repeated patterns. Order functions bottom-up with callees before callers; separate non-obvious logical blocks with a blank line and a brief comment.
- Keep templates functional without JavaScript. Preserve contextual escaping and use HTMX only as progressive enhancement.
- Keep Restop read-only and unauthenticated: do not add HTTP credentials or mutations, change the loopback default, or log repository paths, credentials, or download query strings.
- Use conventional commits with only `feat`, `fix`, `docs`, `refactor`, or `chore`; add a brief body when intent is unclear.
