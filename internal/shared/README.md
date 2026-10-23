# moncommon

Packages monbooru and monloader both carry at `internal/shared/` in each tree:

- `desktop` - the per-OS side of the `-desktop` profile.
- `logx` - the leveled stdlib logger.
- `contract` - the contract the two apps exchange over `/api/v1`.
- `session` - the browser sessions, their CSRF tokens, the session cookie, and the login gate.
- `pairing` - the pending pairing requests a peer polls, the pairing routes.
- `apitoken` - the named bearer tokens that open `/api/v1`.
- `fsx` - the atomic and durable file writes.
- `webui` - the frontend both apps share: htmx, the base `base.css` and `base.js` served at `/static/shared/`, the `webui/*` templates, the theme tokens the shared files read, and the theme folder loader.