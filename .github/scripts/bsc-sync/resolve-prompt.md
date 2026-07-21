You are resolving git merge conflicts produced while merging an upstream
**bnb-chain/bsc** release tag into **chiliz-chain** (Chiliz Chain v2, "CC2"),
a customised fork of BSC. You are running headless inside a GitHub Actions
job. There is no human to ask — make the best decision you can and record
your confidence honestly.

## Required reading

Before touching anything, read `CLAUDE.md` at the repository root. It is the
authoritative description of this fork: the **Invariants** section lists
Chiliz-specific code that must NEVER be lost to upstream, the
**Safe-to-override** section lists areas where upstream almost always wins,
and the **Grey areas** section lists changes that need human judgment. Treat
those three sections as your decision policy.

## Your task

The working tree is mid-merge with conflicts. The conflicted files are:

{{FILES}}

For each conflicted file:

1. Inspect both sides. Useful commands:
   - `git diff` — see the conflict hunks
   - `git log --oneline -5 {{TAG}}` — what the upstream tag changed
   - `git show :1:<file>` / `:2:<file>` / `:3:<file>` — base / ours / theirs
2. Decide the resolution using CLAUDE.md as policy:
   - A Chiliz **invariant** is involved → keep the Chiliz behaviour while
     still integrating any unrelated upstream changes around it.
   - A **safe-to-override** area → take upstream.
   - Otherwise → integrate both sides as faithfully as you can.
3. Edit the file to its final, conflict-free state. Remove ALL conflict
   markers (`<<<<<<<`, `=======`, `>>>>>>>`) for anything you resolve.

## Confidence annotation — this is critical

Classify every file you touch:

- 🟢 **high** — you are confident the resolution is correct. Leave the code
  clean.
- 🟡 **low** — you resolved it but a human must double-check (a grey-area
  file, an invariant you only partially understood, an ambiguous overlap).
  Leave the code conflict-free BUT add a comment on the line(s) in question
  using the file's comment syntax, exactly:
  `TODO: Claude flagged, please review — <one-line reason>`
- 🔴 **unresolved** — you genuinely cannot resolve it safely. Leave the raw
  conflict markers in place untouched so a human resolves it.

When in doubt between high and low, choose **low**. Never silently guess on
an invariant.

## Output — write a JSON report

After resolving, write a JSON file to exactly this path:

    {{REPORT_PATH}}

with this shape (and nothing else in the file):

```json
{
  "tag": "{{TAG}}",
  "resolutions": [
    {
      "file": "consensus/parlia/parlia.go",
      "confidence": "high",
      "summary": "Kept Chiliz distributeIncoming branching; took upstream's gas accounting refactor around it."
    }
  ]
}
```

`confidence` must be one of `high`, `low`, `unresolved`. Include one entry
per conflicted file listed above. Keep each `summary` to one sentence stating
what you kept from each side and why. Do not run `git add`, `git commit`, or
`git merge` — the surrounding script handles staging and committing.
