# Documentation website

Mintlify-inspired static documentation, built without runtime dependencies.

```sh
python3 website/build.py
python3 -m http.server 4173 --directory website/dist
```

Publish `website/dist` on any static host. References are rendered directly from
`docs/{API,MCP,DEPLOY,SECURITY,BENCHMARKS}.md`; curated guides live in `content/`.
The source repository is private; publishing docs must not change its visibility.
The status page is dated and should be updated alongside innernet handoffs.
