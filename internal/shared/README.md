# moncommon

Packages monbooru and monloader both carry at `internal/shared/` in each tree:

- `desktop` - the per-OS side of the `-desktop` profile.
- `logx` - the leveled stdlib logger.
- `contract` - the contract the two apps exchange over `/api/v1`.
- `session` - the browser sessions and their CSRF tokens.
- `pairing` - the pending pairing requests a peer polls