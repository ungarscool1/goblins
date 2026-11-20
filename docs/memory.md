# Engineering memory

Agents start each run knowing only the ticket and the code. A project's memory adds what it
learned and decided before: Goblins' own **memories**, and documents from **knowledge sources**
people maintain. It is selected per run, cited, bounded, and given to agents as data.

## Configuration

```yaml
knowledge:
  docs:
    type: files
    options: {path: /srv/knowledge/app}          # Markdown under this directory only
  wiki:
    type: outline
    options: {url: https://docs.example.com, token_env: OUTLINE_TOKEN, collections: [<collection id>]}
  confluence:
    type: confluence
    options: {url: https://example.atlassian.net, email: me@example.com, token_env: CONFLUENCE_TOKEN,
              spaces: [ENG]}
projects:
  - id: app
    memory:
      enabled: true
      sources: [docs, wiki]
      required: [docs]          # a run stops if it cannot be read; others are skipped and noted
      budget: 8192              # bytes of a prompt, 8 KiB by default
      repository: [README.md, "docs/**/*.md"]   # the repository's own docs, as source "repo"
```

`repository` reads the project's own documentation through its SCM's API, at the commit the task
works from (the latest of the base branch before the first implementation), with no checkout:
links, submodules and paths outside the repository are not files there. Patterns are relative,
and `dir/**/*.md` matches Markdown at any depth under `dir`. Files are fetched once by content
and kept in memory, so later runs read only what changed. A search reads at most 500 matching
files of 256 KiB each, and fetches at most 100 new ones. It needs the `scm.files` capability,
which the GitHub provider has; name `repo` in `required` to make it required.

Without sources, a project with `enabled: true` uses its accepted memories only. No index, model,
vector database or hosted service is needed.

## What an agent is given

Before each planner, implementer and reviewer run, Goblins extracts plain search words from the
task and its prompt, and takes the most relevant entries that fit the budget:

- **accepted** memories of the project's namespace and of the installation's, which are not
  expired;
- documents the sources return for those words.

Each entry has a reference, such as `[memory:m-3f2a]`, `[docs:adr/0004.md]` or
`[repo:docs/adr/0004.md]`, and a citation:
who accepted it and when it was verified, or the document's revision, date and link. The block
says it is background that may be out of date, and that the ticket, the approved design and the
code win where they disagree. Agents are asked to cite what they rely on. The transcript records
what each run was given (`memory` entries), and which sources were skipped.

**A memory whose source changed is flagged.** When a memory names a document with its revision,
as `--source docs:adr/0004.md@<revision>` (the reference and the revision a run's citation
shows), and that document comes back at another revision, the agent is told the source changed
and to check it, and the run's record says so. Nothing is changed or dropped: a person decides,
with `accept` again, `supersede` or `forget`.

## Learning, and who decides

After a commit is published, Goblins records a **proposed** memory: the task, the start of its
approved design, the commit and the pull request. A proposal is never given to agents. A person
accepts it, or forgets it:

```bash
goblins memory list   --project app
goblins memory accept --project app --id p-t-1234-abcdef --as alice
goblins memory add    --project app --kind decision --title "One queue per tenant" \
                      --body "Tenants never share a queue; see ADR 4." --source https://... --as alice
goblins memory supersede --project app --id m-old --by m-new
goblins memory forget --project app --id m-wrong
goblins memory search --project app --text "tenant queue"   # what agents would be given
goblins memory show --project app --id m-3f2a                # one memory and its provenance
goblins memory sources --project app --text "tenant queue"  # ask each source, as a run would
goblins memory export --project app > app-memory.jsonl
goblins memory import --project app < app-memory.jsonl
```

Without `--project`, commands act on the installation-wide namespace. Nothing an agent writes,
and nothing a ticket or comment says, becomes a memory agents are told about without a person.
A superseded memory is kept, for its history, and never given.

## Isolation and safety

- **Namespaces** are `<installation>/<project>` and `<installation>`. A run only ever reads its own
  project's and its installation's, and `import` writes into the namespace given on the command
  line, whatever the file says. Keep work and personal installations apart, as for everything else.
- **Sources read only**, within their allow-lists: a directory opened so that neither links nor
  `..` can leave it, the repository's listed paths through the SCM's API, Outline collections,
  Confluence spaces. Use read-only credentials scoped to
  what agents may see: a source returns what its credentials can read.
- **Only plain words** reach Outline's search and Confluence's CQL, and space keys are validated,
  so a ticket cannot change a query.
- **Documents and memories are untrusted data** inside a delimiter they cannot predict, like
  tickets. They cannot grant a tool, a permission or an approval.
- A required source that fails stops the run with the reason, rather than letting agents work
  without what the project said they need.

## Not yet

Writing back to a wiki or a docs pull request, semantic search, and automatic consolidation are
later work. Sources other than the repository are queried at each run, with no cache or change
feed; a memory's source is only checked when a run's search returns it.
