# The WebSim platform API

Code inside a published WebSim project can call these. They are available on the
global `websim` object with no imports, no keys, and no setup — the platform
injects them. Use them instead of reaching for an external service, and never
put an API key in a project to do something this can already do.

```js
// AI — chat completion (returns { role: "assistant", content: string })
const msg = await websim.chat.completions.create({
  messages: [{ role: "user", content: "..." }], // role: "user" | "assistant" | "system"
  json: true, // optional: ask for a JSON-only answer (then JSON.parse(msg.content))
});

// AI — image generation (returns { url })
const img = await websim.imageGen({ prompt: "...", aspect_ratio: "1:1" /* optional: width, height, seed, transparent */ });

// AI — text to speech (returns { url } of audio)
const speech = await websim.textToSpeech({ text: "...", voice: "en-male" /* e.g. en-male, en-female, it-male */ });

// Identity & context
const user = await websim.getCurrentUser();      // { id, username, avatar_url }
const project = await websim.getCurrentProject(); // { id, title, description }

// Comments (the project's social feed; also usable as simple storage)
await websim.postComment({ content: "markdown **content**" }); // rate limit: 5/min
websim.addEventListener("comment:created", (data) => { /* live updates */ });
const res = await fetch(`/api/v1/projects/${project.id}/comments?first=50&sort_by=best`);
const { comments } = await res.json(); // { data: [{ comment, ... }], meta }
```

## Using them well

Every call is async and can fail. Wrap them, show the person something while
they wait, and degrade gracefully — a project whose whole UI depends on an
unhandled `imageGen` promise looks broken when the call is slow.

`postComment` is rate limited to 5 per minute. If you use comments as storage,
batch writes and expect writes to be rejected.

`getCurrentUser` can be used for per-user state, but remember the comment feed is
public: anything stored there is visible to everyone.

These are the only credentials-free capabilities available. There is no server
you control, no environment variables, and no private storage.
