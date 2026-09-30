# Working agreements

## Git: I commit, you push

I stage and commit. **I do not push.** You run the push yourself:

```bash
cd ~/Personal/chipmank && git push -u origin main
```

Do not add a `git push`, and do not attempt to work around a blocked push
(SSH agent, credentials, HTTPS fallback). Report the commit SHA and stop.

Pushes in this environment fail anyway: the SSH key is passphrase-protected and
there is no agent in WSL, so `ssh` hangs on a prompt unless `BatchMode=yes` is set.

## This repo must stay independent

`/home/aucxley` is a git repo for a *different* project (`School-Os.git`) that
contains the whole home directory, including this one. Chipmank has its own
`.git` at `~/Personal/chipmank` and must stay that way.

Before any commit, confirm:

```bash
git rev-parse --show-toplevel   # must print /home/aucxley/Personal/chipmank
```

If it prints `/home/aucxley`, you are in the wrong repo. Stop.

## Verify before committing

`tools/verify.sh` runs gofmt, build, vet, unit tests, the pgx integration
tests and the schema/RLS suite. Run it and paste the result before claiming
anything works. Do not commit a claim that has not been run.

## Known environment limits

- No real PostgreSQL. `tools/dbtest/serve.mjs` runs PGlite over the wire protocol.
- PGlite serializes queries, so `FOR UPDATE SKIP LOCKED`, job lease races, outbox
  throughput and pooled tenant reset are **untested**. Do not claim they work.
- Against PGlite, append `default_query_exec_mode=exec` to the DSN. Real
  PostgreSQL must keep pgx's default cached mode.
- No Meta account. Use `cmd/simulator`; never claim a real Cloud API call happened.