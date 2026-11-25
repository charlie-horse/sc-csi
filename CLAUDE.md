# sc-csi: CSI driver for Dell SC Series over Fibre Channel

A Go program and Nix flake that let Kubernetes use volumes on a Dell SC
Series (Compellent) storage array over Fibre Channel. It is a CSI driver
(`cmd/sc-csi`, `internal/`), with plain Kubernetes YAML to deploy it
(`deploy/kubernetes`). `AGENTS.md` is a symlink to this file. How
everything works: [docs/README.md](docs/README.md).

## Access

- Host access (SSH, sudo) follows the flake
  repo's [CLAUDE.md](https://github.com/charlie-horse/flake/blob/main/CLAUDE.md#access).
- No tests against a real array, cluster or host unless the user has
  allowed it for the task.
- Never put IPs, hostnames, passwords, tokens, array serial numbers or
  WWPNs of real hardware in code, logs, docs or a PR. Examples use
  made-up values.

## Changes

- All work goes on a branch and a PR to `main`; nobody merges their own PR.
- `nix flake check` is the gate for merging (`nix/checks.nix`). It runs
  pure unit tests only. Run it before pushing.
- Tests that start the in-process fake array or csi-sanity carry the
  `fakearray` build tag. Run them by hand: `go test -tags fakearray ./...`.
  See [docs/testing.md](docs/testing.md).
- Add no heuristic or pattern-matching checks.

## Documentation policy

1. **The code is the source of truth; docs don't restate it.** Don't describe
   what the code declares (versions, pins, image tags, options, defaults,
   flags, environment variables, metric names): read it from the source.
   Docs hold only what the code can't say: procedures, and rules whose
   breach the code can't prevent. Docs cover only the code in this
   repository, plus the minimum array, fabric and node detail needed to use
   it. Don't describe any specific cluster's layout or hosts.
2. **Docs say how things work now.** No history, dates of past events,
   rationale ("why"), measurements of past runs, plans, decisions, open
   questions, owners, people's names or status notes. Present tense. A
   constraint you must follow is written as a rule.
3. **Tasks live in the project, not in git.** No TODO/FIXME/XXX, follow-ups
   or "later" notes in code or docs.
4. **Where docs live.** Prose goes in `docs/`; [docs/README.md](docs/README.md)
   lists every doc and the topic it owns. Access lives only in this file. A
   directory may keep a `README.md` of at most 15 lines that points into
   `docs/`.
5. **One home per topic.** Edit a topic in its doc and link to it elsewhere.
   A new topic gets a row in docs/README.md.
6. **Text in code follows these rules too.** Comments, docstrings, usage
   text and YAML comments obey rules 1-3 and 8. A comment block is at most
   15 lines and says what the code next to it does; longer prose goes to a
   doc, with a one-line pointer in the code.
7. **Docs change in the same PR as the code.** Moving or renaming a doc or
   heading fixes every link to it.
8. **Plain language, low complexity.** Write so a layperson new to
   Kubernetes storage and Fibre Channel can follow: short sentences, plain
   words, each term of art defined on first use. Keep each doc as short as
   it can be while still usable.
