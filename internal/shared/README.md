# moncommon

Packages monbooru, monloader and monworker carry at `internal/shared/` in each tree:

- `desktop` - the per-OS layer: the `-desktop` profile's paths, single instance, menu entry and tray, and what headless code needs from the OS too (disk usage, process memory, console hiding, the Host guard).
- `logx` - the leveled stdlib logger.
- `contract` - the contract the two apps exchange over `/api/v1`.
- `session` - the browser sessions, their CSRF tokens, the session cookie, and the login gate.
- `pairing` - the pending pairing requests a peer polls, the pairing routes.
- `apitoken` - the named bearer tokens that open `/api/v1`.
- `fsx` - the atomic and durable file writes.
- `worker` - what monbooru and monworker exchange over monworker's `/v1`: the descriptions, the tag call, a client and a fake worker for tests.
- `webui` - the frontend both apps share: htmx, fonts, the base `base.css` and `base.js` served at `/static/shared/`, the `webui/*` templates, the theme tokens the shared files read, and the theme folder loader.