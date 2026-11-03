const dialog = document.querySelector('#search-dialog');
const query = document.querySelector('#query');
const results = document.querySelector('#results');
let index;
async function search() {
  results.replaceChildren();
  try {
    index ||= await fetch('search.json').then(response => {
      if (!response.ok) throw new Error('Search unavailable');
      return response.json();
    });
    const words = query.value.toLowerCase().trim().split(/\s+/).filter(Boolean);
    const matches = index.filter(page => words.every(word => (page.title + ' ' + page.text).toLowerCase().includes(word)));
    if (!matches.length) results.textContent = 'No pages found. Try “snapshot”, “MCP”, or “next steps”.';
    for (const page of matches) {
      const link = document.createElement('a');
      link.href = page.url;
      link.textContent = page.title;
      const preview = document.createElement('small');
      const start = words.length ? Math.max(0, page.text.toLowerCase().indexOf(words[0]) - 45) : 0;
      preview.textContent = page.text.slice(start, start + 155).replace(/[#`*]/g, '') + '…';
      link.append(preview);
      results.append(link);
    }
  } catch { results.textContent = 'Search could not load. Use the documentation sidebar to browse.'; }
}
function openSearch() { dialog.showModal(); query.focus(); search(); }
document.querySelector('#search-open').addEventListener('click', openSearch);
query.addEventListener('input', search);
document.addEventListener('keydown', event => {
  if ((event.metaKey || event.ctrlKey) && event.key.toLowerCase() === 'k') { event.preventDefault(); if (!dialog.open) openSearch(); }
});
dialog.addEventListener('click', event => { if (event.target === dialog) { const r = dialog.getBoundingClientRect(); if (event.clientX < r.left || event.clientX > r.right || event.clientY < r.top || event.clientY > r.bottom) dialog.close(); } });
document.querySelector('#menu').addEventListener('click', event => { const open = document.querySelector('#sidebar').classList.toggle('open'); event.currentTarget.setAttribute('aria-expanded', String(open)); });
for (const button of document.querySelectorAll('.copy')) {
  button.addEventListener('click', async () => {
    try { await navigator.clipboard.writeText(button.closest('.code').querySelector('pre').textContent); button.textContent = 'Copied'; }
    catch { button.textContent = 'Select code to copy'; }
    setTimeout(() => { button.textContent = 'Copy'; }, 2000);
  });
}
