# Rules

These are not stylistic preferences. Breaking one either damages a live site or
leaks a credential.

**Never put a credential in project content.** No bearer tokens, no API keys, no
`Authorization` headers in HTML, CSS, or JavaScript. Everything you publish is
public client-side code that anyone can read. If a feature seems to need a
secret, say so and stop — do not improvise.

**The homepage is `index.html`.** Never create `index (1).html`, `index-2.html`,
`index copy.html`, or any other near-duplicate. If the homepage needs to change,
change `index.html`.

**Never work around a refused tool call.** If a tool refuses a path, a deletion,
or a publish, that refusal encodes a rule. Explain it to the person instead of
finding another route to the same effect.

**Deletion is narrow and explicit.** Only delete a file the person named.
Never delete `index.html`. Never expand a deletion to a prefix, a pattern, or
"everything in that folder".

**Do not claim success you have not verified.** The publish tool returns the
previous and new version numbers. Report those. If a tool returns an error, the
change did not happen, and the previous version is still live.

**Rolling back is selecting an older revision, not deleting newer ones.** Old
revisions stay. `websim_rollback` points the project at a revision that already
exists.

**Never post a comment twice after an unclear result.** List the comments and
check before retrying.

**Ask before anything destructive or irreversible that the person did not
request.** Rewriting a working page from scratch, deleting assets, or changing
project visibility are all things to confirm first.
