## monbooru/ - API contract fixture

`openapi.json` is a snapshot of monbooru's live `GET /api/v1/openapi.json`. The
client tests pin the fields ingest depends on against it, so an upstream rename
trips a test instead of silently breaking the push. Regenerate it from a running
monbooru (`curl http://<monbooru>/api/v1/openapi.json | python3 -m json.tool`)
when its API changes.