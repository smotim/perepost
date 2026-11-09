# Agent skills

Third-party skills for Claude Code, Codex and Cursor. The canonical copies live here;
`.claude/skills` holds relative symlinks to them. They are pinned copies and never update
themselves, on purpose: an agent runs a skill with the developer's permissions, so new text
lands here only after review.

| Skills | Source @ commit | Licence | Local changes |
| --- | --- | --- | --- |
| `go-code-review`, `go-concurrency-review`, `go-context`, `go-defensive-coding`, `go-error-handling`, `go-modernize`, `go-naming`, `go-security-audit`, `go-test-quality`, `go-troubleshooting` | [eduardo-sl/go-agent-skills](https://github.com/eduardo-sl/go-agent-skills) @ `e25b517` | MIT, `LICENSE-go-agent-skills` | `allowed-tools` removed from every skill (it pre-approved `go`, `git` and other commands); `compatibility` quoted in `go-code-review` and `go-security-audit` (upstream YAML does not parse) |

## Checking for updates

Monthly, and before larger Go work:

1. Clone upstream and compare: `git clone --depth 1 <repository> /tmp/skill-up` and
   `diff -r "/tmp/skill-up/skills/(<category>)/<skill>" .agents/skills/<skill>`.
2. Review the new version before replacing:
   - scripts and their network calls (`curl`, `wget`, `npx`, binary downloads);
   - hidden Unicode: `grep -rlP '[\x{200B}-\x{200F}\x{202A}-\x{202E}\x{2060}-\x{2064}\x{FEFF}\x{E0000}-\x{E007F}]' <dir>`;
   - `allowed-tools` in the frontmatter: a skill must not grant itself commands;
   - hooks and agent settings changes;
   - frontmatter YAML validity.
3. Replace the directory, re-apply the local changes from the table, update the commit above.
4. A new skill gets the same review, a table row and a symlink:
   `ln -s ../../.agents/skills/<skill> .claude/skills/<skill>`.

Do not run `npx skills update` over these copies: it pulls unreviewed text and drops the
local changes.
