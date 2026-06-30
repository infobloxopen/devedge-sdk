# devedge-sdk documentation style guide

This guide defines the writing standard for the devedge-sdk documentation under
`docs/content/docs/`. The target is the register of high-quality external developer
documentation (Google Cloud, Stripe): neutral, uniform, reader-centric, and easy to scan.

Apply this guide to every page. It is a checklist, not a suggestion — each rule below is meant
to be verifiable against a page during review. The "Calibration scope" section at the end
records which pages have already been brought up to standard.

## Voice and register

- **Write to the reader, in the second person.** Say "you record an event", not "the developer
  records an event" or "one records an event".
- **Use the present tense and the active voice.** "The dispatcher reads the outbox", not "The
  outbox is read by the dispatcher" or "The dispatcher will read the outbox".
- **Stay neutral.** Do not defend, praise, or editorialize the design. Remove team-facing and
  design-defending framing such as "deliberately", "by design", "out of scope", "the whole
  point", "forbidden", "clean substrate", "the correct shape", "reach for it". State what the
  thing does and what the reader does with it.
- **Describe behavior, not intent.** Replace "we chose X so that Y" with "X gives you Y" or "Use
  X when Y".
- **No first person.** Avoid "we", "our", "I". The product is "the SDK" or "devedge-sdk"; the
  reader is "you".
- **No marketing adjectives.** Drop "powerful", "simple", "elegant", "clean", "batteries-included"
  unless the word is load-bearing and defined. Let the capability speak for itself.

## Page openings: orient before you motivate

Every page opens with a short plain-language paragraph that answers, in this order:

1. **What it is** — a one-sentence definition.
2. **What it does for you** — the concrete benefit to the reader.
3. **When to use it** — the situation that should send a reader here.

Only after that orientation do you introduce the problem, the motivation, or the history. Never
open a page with the problem statement; a reader who lands here may not yet know they have the
problem.

## Headings

- **Use sentence case.** "Delivery guarantees", not "Delivery Guarantees".
- **Use short noun phrases as labels, not thesis statements.** Good: "Dispatch", "Retention",
  "Failure modes". Bad: "Dispatch: a forward cursor over a write-only outbox", "Write-only
  outbox, drop-partition retention, and the CDC seam".
- **Make the table of contents navigable.** A reader scanning only the headings should be able
  to find the section they need. Headings carry no argument; the prose under them does.
- **Keep anchors stable.** Hugo derives the anchor from the heading text. If you rename a
  heading, search the repo for in-page links to its old anchor (`grep -rn '#old-anchor'
  docs/content`) and update them so nothing 404s.
- **Nest by depth, not for emphasis.** `##` for top-level sections, `###` for subsections. Do
  not skip levels.

## Sentences and paragraphs

- **One main idea per sentence.** Split a sentence that joins two ideas with "and which", "so
  that", em-dashes carrying a second clause, or a parenthetical that introduces a new concept.
- **One topic per paragraph.** Lead with the topic sentence; supporting detail follows.
- **Keep idea-density low.** If a sentence needs to be re-read to be parsed, rewrite it. Prefer
  several short sentences over one dense one.
- **Cut compressed shorthand and aphorisms.** Phrases like "no orphan event", "at-least-once +
  idempotency = exactly-once effect", or "per-poll re-lease/mark churn or write+delete vacuum
  bloat" pack the explanation into a slogan. Explain the mechanism in plain words first; you may
  keep a short memorable phrase only after the plain explanation, never as a substitute for it.

## Lists and tables

- **Use a numbered list for a sequence** (steps, an ordered algorithm, a procedure). Never bury
  a multi-step algorithm inside one sentence.
- **Use a bulleted list for an unordered set** (options, properties, related items).
- **Use a table for options or comparisons** — fields with defaults, sentinels mapped to codes,
  backend A vs backend B. Keep one row per item and a consistent column set.
- **Lead each list item with its point.** Front-load the keyword so the list is scannable.

## Progressive disclosure and the inverted pyramid

Order each page so a reader can stop as soon as they have what they need:

1. Orientation (what / why / when).
2. A minimal, runnable example.
3. How it works (the normal path).
4. Guarantees and tradeoffs.
5. Failure modes.
6. Advanced topics and internals — last, or on a separate page.

Put the most important information first within every section, too. A reader who reads only the
first paragraph of a section should get its headline.

## Terminology

- **Define a term on first use, or link to the page that defines it.** Terms such as
  "aggregate", "sidecar", "seam", "poison event", "dead-letter", and "CDC" are not assumed
  knowledge. Either define them in a clause on first use or link to the concept page.
- **Use one term for one thing, consistently.** Do not alternate between "dispatcher" and
  "consumer" for the same component on the same page.
- **Link related concepts** with a relative Hugo link (`../aggregates/`), and keep a **See also**
  section at the foot of concept pages.

## Plain language

- **No insider jargon without a definition.** If a phrase would only land for someone who already
  built the system, rewrite it for someone who has not.
- **No cleverness for its own sake.** Wordplay, in-jokes, and rhetorical flourishes slow readers
  down. Prefer the plain statement.
- **Expand an acronym on first use** unless it is universally known in the domain (gRPC, HTTP,
  SQL, ID).

## Caveats and warnings: use callouts, not buried prose

Honest caveats and tradeoffs are valuable and must be preserved. Surface them, do not hide them
in the middle of a paragraph. Use the Hextra callout shortcode:

```md
{{< callout type="warning" >}}
**Drop only partitions that are fully behind the dispatch cursor.** Otherwise the in-process
dispatcher can lose an event it has not yet consumed.
{{< /callout >}}
```

- `type="info"` — a note, a tip, a clarification.
- `type="warning"` — a caveat, a footgun, a "you must" constraint.
- `type="error"` — a hard prohibition or a destructive action.

Start a callout with a **bold lead sentence** stating the point, then explain. Reserve callouts
for genuine caveats; do not wrap ordinary prose in them.

A cluster of tradeoffs can also live under a `## Tradeoffs` or `## Guarantees and tradeoffs`
subsection. Either way, never delete a real caveat when normalizing voice — relocate it.

## Register by Diátaxis genre

The docs follow the Diátaxis structure (`concepts`, `explanation`, `how-to`, `reference`,
`tutorial`, `getting-started`). Keep that structure; do not move or rename pages. Each genre
keeps the neutral voice above, with these emphases:

- **Reference** (`reference/`): terse, uniform, and complete. Lead each symbol with one sentence
  of what it is, then a table of fields or parameters with defaults. Same shape on every page.
  No narrative, no motivation — the reader is looking something up.
- **How-to** (`how-to/`): a task recipe. Title is a task ("Deploy a service"). Open with the
  goal and prerequisites, then numbered steps, then how to verify the result. One way to do the
  task; link alternatives, do not inline them.
- **Tutorial** (`tutorial/`, `getting-started/`): learning by doing. Friendly but still neutral.
  A single happy path the reader follows start to finish; defer alternatives and edge cases to
  how-to and reference pages so the learner is never blocked by a choice.
- **Concept** (`concepts/`) and **explanation** (`explanation/`): plain expository prose.
  Orient, then explain the mechanism and the tradeoffs. This is where progressive disclosure
  matters most — keep deep internals last.

## Front matter, links, and code

- **Preserve Hugo front matter** on every page (`title`, `weight`, `menu`, and any others). Do
  not change `weight` values; they set page order.
- **Keep `_index.md` section pages working.** They define each section landing page.
- **Preserve internal links.** Use relative links between pages (`../events/`,
  `../../reference/server/`). After any heading rename, fix in-repo anchor links to it.
- **Preserve code samples exactly.** They reference a real API. You may add explanatory prose or
  lists around a sample and fix a broken fence, but do not change code semantics, identifiers, or
  output.
- **Do not commit build output.** `docs/public/` and `docs/resources/` are generated; never edit
  or commit them.

## Before / after (from `concepts/events.md`)

The events page is the worked example for this initiative. Two representative fixes:

**Page opening — problem-first → orientation-first.**

Before:

> Some rules span **more than one aggregate**: "when a user is suspended, revoke that user's API
> keys"; "when an account is closed, suspend its users". The suspended user and its API keys are
> different aggregates — each its own consistency boundary — so the reaction **cannot** be one
> transaction (that would be a forbidden two-aggregate write) ...

After:

> Events let one part of your service react to a change in another part without sharing a
> database transaction. You record a domain event in the same commit as the change that caused
> it, and a dispatcher delivers that event to a handler that runs its own follow-up work.
>
> Use events when a rule spans more than one [aggregate](../aggregates/) — for example, "when a
> user is suspended, revoke that user's API keys."

**Heading — thesis statement → label.**

Before: `## Dispatch: a forward cursor over a write-only outbox`

After: `## Dispatching events`

**Caveat — buried in prose → callout.**

Before (mid-paragraph): "... **Drop only partitions that are both older than the retention window
AND fully behind the dispatch cursor**, so the in-process dispatcher never loses an event ..."

After:

> {{< callout type="warning" >}}
> **Drop only partitions that are both older than the retention window and fully behind the
> dispatch cursor.** Otherwise the in-process dispatcher can lose an event it has not yet
> consumed. Size the retention window comfortably longer than the dispatcher could ever fall
> behind.
> {{< /callout >}}

## Calibration scope

This standard was calibrated on a small set of pages first, so a human can approve the direction
before it rolls out to the rest. Pages already brought up to standard:

- `concepts/events.md`
- `concepts/transactions.md`
- `reference/server.md`

The remaining pages under `docs/content/docs/` will be normalized to this guide in a follow-up
change, one section at a time.
