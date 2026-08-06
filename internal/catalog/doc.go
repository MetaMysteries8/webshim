package catalog

// Regenerating fallback.json
//
// The embedded snapshot is a deliberately small subset of models.dev: Charm
// Hyper in full, plus the most recent tool-calling models from the major
// direct providers. It is an offline safety net, not a mirror -- aggregators
// like OpenRouter, Azure, and Bedrock list thousands of models and are left to
// the live fetch.
//
// To refresh it:
//
//	curl -o /tmp/models.json https://models.dev/api.json
//	python - <<'PY'
//	import json, io
//	src = json.load(io.open('/tmp/models.json', encoding='utf-8'))
//	WHOLE  = ['hyper']
//	CAPPED = {'anthropic': 12, 'openai': 12, 'google': 10, 'deepseek': 6,
//	          'xai': 5, 'groq': 6, 'mistral': 8, 'moonshotai': 6,
//	          'zhipuai': 6, 'cerebras': 4, 'openrouter': 15}
//	FIELDS = ['id','name','family','tool_call','reasoning','structured_output',
//	          'attachment','temperature','modalities','limit','cost',
//	          'release_date','reasoning_options']
//	out = {}
//	for pid in WHOLE + list(CAPPED):
//	    p = src.get(pid)
//	    if not p: continue
//	    models = [(k, v) for k, v in p.get('models', {}).items() if v.get('tool_call')]
//	    models.sort(key=lambda kv: kv[1].get('release_date', ''), reverse=True)
//	    if pid in CAPPED: models = models[:CAPPED[pid]]
//	    if not models: continue
//	    entry = {k: p[k] for k in ('id','name','env','api','doc') if k in p}
//	    entry['models'] = {k: {f: v[f] for f in FIELDS if f in v} for k, v in models}
//	    out[pid] = entry
//	with io.open('internal/catalog/fallback.json', 'w', encoding='utf-8') as f:
//	    json.dump(out, f, indent=1, sort_keys=True, ensure_ascii=False)
//	    f.write('\n')
//	PY
//
// Then run `go test ./internal/catalog/` -- TestEmbeddedFallbackIsUsable checks
// that the result still parses and still contains a usable default.
