"""Build the documentation site with only the Python standard library."""
import html
import json
import re
from pathlib import Path

ROOT = Path(__file__).resolve().parent
REPO = ROOT.parent
OUT = ROOT / 'dist'
PAGES = [
    ('introduction', 'Introduction', 'Start here', ROOT/'content/introduction.md'),
    ('quickstart', 'Quickstart', 'Start here', ROOT/'content/quickstart.md'),
    ('architecture', 'Architecture', 'Concepts', ROOT/'content/architecture.md'),
    ('lifecycle', 'Lifecycle & snapshots', 'Concepts', ROOT/'content/lifecycle.md'),
    ('api', 'HTTP & WebSocket API', 'Reference', REPO/'docs/API.md'),
    ('mcp', 'MCP tools', 'Reference', REPO/'docs/MCP.md'),
    ('configuration', 'Configuration', 'Reference', ROOT/'content/configuration.md'),
    ('deploy', 'Deployment', 'Operate', REPO/'docs/DEPLOY.md'),
    ('security', 'Security & isolation', 'Operate', REPO/'docs/SECURITY.md'),
    ('benchmarks', 'Benchmarks', 'Operate', REPO/'docs/BENCHMARKS.md'),
    ('development', 'Development', 'Contribute', ROOT/'content/development.md'),
    ('roadmap', 'Status & next steps', 'Contribute', ROOT/'content/roadmap.md'),
]
LINKS = {str(path.relative_to(REPO)): slug+'.html' for slug, _, _, path in PAGES}
LINKS.update({path.name: slug+'.html' for slug, _, _, path in PAGES})
def inline(s):
    s = html.escape(s)
    s = re.sub(r'`([^`]+)`', r'<code>\1</code>', s)
    s = re.sub(r'\*\*([^*]+)\*\*', r'<strong>\1</strong>', s)
    def link(m):
        url = html.unescape(m[2])
        url = LINKS.get(url, url)
        if not (url.startswith(('https://', 'http://', '#')) or url.endswith('.html')):
            url = 'https://github.com/mondeep31/agent-comp/blob/main/' + url
        return '<a href="'+html.escape(url, quote=True)+'">'+m[1]+'</a>'
    return re.sub(r'\[([^\]]+)\]\(([^)]+)\)', link, s)

def render(md):
    lines=md.splitlines(); out=[]; headings=[]; i=0; counts={}
    while i<len(lines):
        l=lines[i]
        if not l.strip(): i+=1; continue
        if l.lstrip().startswith('```'):
            indent=len(l)-len(l.lstrip()); l=l.lstrip()
            lang=l[3:]; block=[]; i+=1
            while i<len(lines) and not lines[i].lstrip().startswith('```'): block.append(lines[i][indent:]); i+=1
            out.append('<div class="code"><div class="codebar"><span>'+html.escape(lang or 'terminal')+'</span><button class="copy" aria-label="Copy code">Copy</button></div><pre><code>'+html.escape('\n'.join(block))+'</code></pre></div>'); i+=1; continue
        m=re.match(r'^(#{1,6}) (.+)',l)
        if m:
            n=len(m[1]); label=m[2]; anchor=re.sub(r'[^a-z0-9]+','-',label.lower()).strip('-'); counts[anchor]=counts.get(anchor,0)+1
            if counts[anchor]>1: anchor+='-'+str(counts[anchor])
            out.append(f'<h{n} id="{anchor}">{inline(label)}</h{n}>')
            if n in (2,3): headings.append((n,label,anchor))
            i+=1; continue
        if l.startswith('|') and i+1<len(lines) and re.match(r'^\|[ :|-]+\|$',lines[i+1]):
            rows=[l]; i+=2
            while i<len(lines) and lines[i].startswith('|'): rows.append(lines[i]); i+=1
            out.append('<div class="table-wrap"><table>')
            for r,row in enumerate(rows):
                tag='th' if r==0 else 'td'; out.append('<tr>'+''.join(f'<{tag}>{inline(c.strip())}</{tag}>' for c in re.split(r'\|(?=(?:[^`]*`[^`]*`)*[^`]*$)', row.strip('|')))+'</tr>')
            out.append('</table></div>'); continue
        if re.match(r'^(- |\d+\. )',l):
            ordered=bool(re.match(r'^\d',l)); tag='ol' if ordered else 'ul'; out.append('<'+tag+'>')
            while i<len(lines) and re.match(r'^(- |\d+\. )',lines[i]):
                item=re.sub(r'^(- |\d+\. )','',lines[i]); i+=1
                while i<len(lines) and lines[i].startswith('  ') and not lines[i].lstrip().startswith('```'): item+=' '+lines[i].strip(); i+=1
                out.append('<li>'+inline(item)+'</li>')
            out.append('</'+tag+'>'); continue
        para=[l]; i+=1
        while i<len(lines) and lines[i].strip() and not re.match(r'^(#|```|\||- |\d+\. )',lines[i]): para.append(lines[i]); i+=1
        out.append('<p>'+inline(' '.join(para))+'</p>')
    return '\n'.join(out), headings

OUT.mkdir(exist_ok=True)
for name in ('style.css','app.js'):
    (OUT/name).write_text((ROOT/'assets'/name).read_text())
search=[]
for index,(slug,title,group,path) in enumerate(PAGES):
    md=path.read_text()
    # Publish generic network policy, not the development host's LAN topology.
    md=md.replace('the LAN `10.253.0.0/24`','the host LAN')
    body,heads=render(md)
    nav=''; last=None
    for s,t,g,_ in PAGES:
        if last!=g: nav+='<div class="nav-group">'+g+'</div>'; last=g
        nav+=f'<a href="{s}.html" '+('aria-current="page"' if s==slug else '')+'>'+t+'</a>'
    toc=''.join(f'<a class="depth-{n}" href="#{a}">{html.escape(t)}</a>' for n,t,a in heads)
    pager=''
    for j,label in ((index-1,'← Previous'),(index+1,'Next →')):
        if 0<=j<len(PAGES): pager+=f'<a href="{PAGES[j][0]}.html"><small>{label}</small>{PAGES[j][1]}</a>'
    page=f'''<!doctype html><html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1"><meta name="description" content="agent-comp documentation: isolated Firecracker computers for AI agents. {title}."><title>{title} · agent-comp docs</title><link rel="stylesheet" href="style.css"><script defer src="app.js"></script></head><body>
<a class="skip" href="#main">Skip to content</a><header><a class="brand" href="index.html"><span class="mark">a<span>c</span></span>agent-comp <span class="docs-label">/ docs</span></a><button id="search-open">⌕ <span>Search documentation</span><kbd>⌘ K</kbd></button><a class="github" href="https://github.com/mondeep31/agent-comp">GitHub ↗</a><button id="menu" aria-label="Toggle navigation" aria-expanded="false">☰</button></header>
<div class="layout"><nav id="sidebar" aria-label="Documentation"><div class="version"><span class="dot"></span>v1 · Development</div>{nav}<div class="sidebar-note">A real Linux computer.<br>A fresh start for every agent.</div></nav><main id="main"><div class="eyebrow">{group}</div>{body}<div class="pager">{pager}</div><footer>agent-comp documentation <span>Source of truth: code + project memory</span></footer></main><aside aria-label="On this page"><div>On this page</div>{toc}<a class="edit" href="https://github.com/mondeep31/agent-comp/blob/main/{path.relative_to(REPO)}">Edit this page ↗</a></aside></div>
<dialog id="search-dialog"><form method="dialog"><label for="query">Search documentation</label><button aria-label="Close search">Esc</button></form><input id="query" type="search" placeholder="Search APIs, setup, next steps…" autocomplete="off"><div id="results" aria-live="polite"></div></dialog></body></html>'''
    (OUT/(slug+'.html')).write_text(page)
    if index==0: (OUT/'index.html').write_text(page)
    search.append({'title':title,'url':slug+'.html','text':md})
(OUT/'search.json').write_text(json.dumps(search))
(OUT/'.nojekyll').touch()
print(f'Built {len(PAGES)} documentation pages in {OUT}')
