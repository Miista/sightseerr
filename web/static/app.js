'use strict';

// Four views on one page: Discover (the wall), a title's page (with the
// trail that led there), Library (rate what you've watched) and Blocked.
// The server decides who you are and checks everything again; what happens
// here is only for the person.

const $ = (id) => document.getElementById(id);
const IMG = 'https://image.tmdb.org/t/p/';
const view = $('view');

let me = null;        // /api/me
let renderSeq = 0;    // a newer navigation makes older answers stale

// h builds an element: h('a', {class: 'card', href: '/'}, child, 'text').
function h(tag, attrs, ...kids) {
  const el = document.createElement(tag);
  for (const [k, v] of Object.entries(attrs || {})) {
    if (v === undefined || v === null || v === false) continue;
    if (k === 'class') el.className = v;
    else if (k === 'text') el.textContent = v;
    else if (k.startsWith('on')) el.addEventListener(k.slice(2), v);
    else el.setAttribute(k, v === true ? '' : v);
  }
  for (const kid of kids.flat()) {
    if (kid === null || kid === undefined || kid === false) continue;
    el.append(kid instanceof Node ? kid : document.createTextNode(String(kid)));
  }
  return el;
}

// svg turns fixed icon markup into an element; h() can't, since SVG elements
// need their own namespace. Only ever given literals from this file.
function svg(markup) {
  const tpl = document.createElement('template');
  tpl.innerHTML = markup;
  return tpl.content.firstElementChild;
}

async function api(method, path, body) {
  const opts = { method, headers: {} };
  if (body !== undefined) {
    opts.headers['Content-Type'] = 'application/json';
    opts.body = JSON.stringify(body);
  }
  const res = await fetch(path, opts);
  if (res.status === 401) { location.reload(); throw new Error('Signed out'); }
  const data = await res.json().catch(() => ({}));
  if (!res.ok) throw new Error(data.error || `Something went wrong (${res.status})`);
  return data;
}

const keyOf = (t) => `${t.type}:${t.id}`;
const titleHref = (t, via) => `/${t.type}/${t.id}` + (via.length ? `?via=${via.map(encodeURIComponent).join(',')}` : '');

// ---------- Navigation ----------

// Where each page was scrolled to, so going back (or returning to Discover
// from Library) lands where you left off. Kept for the browser session.
const scrolls = JSON.parse(sessionStorage.getItem('spor.scrolls') || '{}');
const here = () => location.pathname + location.search;
const saveScroll = () => {
  scrolls[here()] = Math.round(scrollY);
  sessionStorage.setItem('spor.scrolls', JSON.stringify(scrolls));
};
history.scrollRestoration = 'manual'; // the page renders after fetching, so do it ourselves

// The last place in Discover (the wall, or a title with its trail): the
// Discover tab brings you back there from Library rather than to the wall.
let lastDiscover = sessionStorage.getItem('spor.discover') || '/';
const inDiscover = (path) => path === '/' || /^\/(movie|tv)\/\d+$/.test(path);

async function go(url, { restore = false } = {}) {
  saveScroll();
  history.pushState(null, '', url);
  window.scrollTo(0, 0);
  await render();
  if (restore) window.scrollTo(0, scrolls[here()] || 0);
}

document.addEventListener('click', (e) => {
  const a = e.target.closest('a');
  if (!a || e.defaultPrevented || e.button !== 0 || e.metaKey || e.ctrlKey || e.shiftKey || e.altKey) return;
  const url = new URL(a.href, location.href);
  if (url.origin !== location.origin || url.pathname.startsWith('/auth/')) return;
  e.preventDefault();
  // The Discover tab, from outside Discover: back to where you were there.
  // From inside Discover it goes to the wall, like tapping an iOS tab twice.
  if (a.dataset.tab === 'wall' && !inDiscover(location.pathname)) return go(lastDiscover, { restore: true });
  go(url.pathname + url.search);
});
window.addEventListener('popstate', async () => {
  await render();
  window.scrollTo(0, scrolls[here()] || 0);
});
// Remember the spot continuously too, so a reload or Back from elsewhere finds it.
let scrollTimer = null;
window.addEventListener('scroll', () => { clearTimeout(scrollTimer); scrollTimer = setTimeout(saveScroll, 150); }, { passive: true });

// Sticky section headers sit right under the title bar, whose height differs
// between phone and desktop; keep --topbar in step with it.
const topbar = document.querySelector('.top');
const setTopbar = () => document.documentElement.style.setProperty('--topbar', `${topbar.offsetHeight}px`);
new ResizeObserver(setTopbar).observe(topbar);
setTopbar();

function setTab(name) {
  for (const a of document.querySelectorAll('a[data-tab]')) a.classList.toggle('on', a.dataset.tab === name);
}

async function render() {
  const seq = ++renderSeq;
  closeMenu();
  for (const dlg of document.querySelectorAll('dialog[open]')) dlg.close();
  document.title = 'spor';
  const path = location.pathname;
  const m = path.match(/^\/(movie|tv)\/(\d+)$/);
  if (!me) {
    try { me = await api('GET', '/api/me'); } catch (err) { return showNote('Could not reach spor', err.message); }
    $('me').textContent = me.name || '';
  }
  if (seq !== renderSeq) return;
  if (!me.linked) {
    setTab('');
    return showNote('Your account isn\'t linked', `${me.reason} Ask whoever runs spor to link it.`);
  }
  if (inDiscover(path)) {
    lastDiscover = here();
    sessionStorage.setItem('spor.discover', lastDiscover);
  }
  // A title page is part of Discover, so the Discover tab stays lit there.
  if (m) { setTab('wall'); return renderTitle(seq, m[1], Number(m[2]), new URLSearchParams(location.search).get('via') || ''); }
  if (path === '/library') { setTab('library'); return renderLibrary(seq); }
  if (path === '/blocked') { setTab(''); return renderBlocked(seq); }
  setTab('wall');
  return renderWall(seq);
}

function showNote(title, text) {
  view.replaceChildren(h('div', { class: 'note' }, h('strong', { text: title }), text));
}

// ---------- Pieces ----------

function posterEl(t) {
  const p = h('div', { class: 'poster' });
  if (t.poster) p.append(h('img', { src: IMG + 'w342' + t.poster, alt: '', loading: 'lazy', decoding: 'async' }));
  else p.append(h('div', { class: 'noimg', text: t.title }));
  p.append(h('span', { class: `kind ${t.type}`, text: t.type === 'tv' ? 'Series' : 'Movie' }));
  return p;
}

function gridSkeleton(n, extra) {
  const g = h('div', { class: 'grid', 'aria-busy': 'true' });
  for (let i = 0; i < n; i++) {
    g.append(h('div', { class: 'card' },
      h('div', { class: 'poster skel' }),
      h('div', { class: 'skel line' }),
      h('div', { class: 'skel line short' }),
      extra ? h('div', { class: 'skel line', style: 'height:30px' }) : null));
  }
  return g;
}

function header(title, lede) {
  return [h('h1', { text: title }), lede ? h('p', { class: 'lede' }, lede) : null];
}

// ---------- Context menu (long-press on touch, right-click elsewhere) ----------

const menu = $('menu');
let menuFor = null;

function closeMenu() { menu.hidden = true; menuFor = null; }

function openMenu(x, y, t, items) {
  menuFor = t;
  menu.replaceChildren(h('div', { class: 'head', text: t.title }),
    ...items.map((it) => h('button', { class: it.danger ? 'danger' : '', role: 'menuitem', text: it.label,
      onclick: () => { closeMenu(); it.run(); } })));
  menu.hidden = false;
  const r = menu.getBoundingClientRect();
  menu.style.left = Math.max(8, Math.min(x, innerWidth - r.width - 8)) + 'px';
  menu.style.top = Math.max(8, Math.min(y, innerHeight - r.height - 8)) + 'px';
  menu.querySelector('button')?.focus({ preventScroll: true });
}

document.addEventListener('pointerdown', (e) => {
  if (!menu.hidden && !menu.contains(e.target)) closeMenu();
  // An open trail dropdown closes on a tap anywhere else.
  for (const d of document.querySelectorAll('details.trail[open]')) if (!d.contains(e.target)) d.open = false;
});
document.addEventListener('keydown', (e) => { if (e.key === 'Escape') closeMenu(); });
window.addEventListener('scroll', closeMenu, { passive: true });

// pressable opens the menu on a long press or a right-click, and swallows the
// click that ends a long press so it doesn't also open the title.
function pressable(el, t, items) {
  let timer = null, startX = 0, startY = 0, fired = false, lastOpen = 0;
  const open = (x, y) => {
    if (Date.now() - lastOpen < 600) return; // touch long-press can also send contextmenu
    lastOpen = Date.now();
    fired = true;
    openMenu(x, y, t, items);
  };
  el.addEventListener('pointerdown', (e) => {
    fired = false;
    if (e.pointerType === 'mouse') return;
    startX = e.clientX; startY = e.clientY;
    timer = setTimeout(() => { navigator.vibrate?.(10); open(startX, startY); }, 500);
  });
  el.addEventListener('pointermove', (e) => {
    if (timer && Math.hypot(e.clientX - startX, e.clientY - startY) > 10) { clearTimeout(timer); timer = null; }
  });
  for (const ev of ['pointerup', 'pointercancel', 'pointerleave']) el.addEventListener(ev, () => { clearTimeout(timer); timer = null; });
  el.addEventListener('contextmenu', (e) => { e.preventDefault(); open(e.clientX, e.clientY); });
  el.addEventListener('click', (e) => { if (fired) { e.preventDefault(); e.stopPropagation(); fired = false; } }, true);
}

// blockItem hides the card at once; Undo puts it back where it was.
function blockItem(t, card) {
  return {
    label: 'Not interested — block', danger: true,
    run: async () => {
      const parent = card.parentNode, next = card.nextSibling;
      card.remove();
      try {
        await api('POST', '/api/block', { type: t.type, id: t.id });
      } catch (err) {
        parent?.insertBefore(card, next);
        return toast(err.message, { error: true });
      }
      toast(`Blocked ${t.title}`, {
        undo: async () => {
          try {
            await api('POST', '/api/unblock', { type: t.type, id: t.id });
            parent?.insertBefore(card, next);
          } catch (err) { toast(err.message, { error: true }); }
        },
      });
    },
  };
}

// ---------- Toast ----------

let toastTimer = null;
function toast(msg, opts = {}) {
  const el = $('toast');
  el.classList.toggle('error', !!opts.error);
  el.replaceChildren(h('span', { text: msg }),
    opts.undo ? h('button', { text: 'Undo', onclick: () => { el.hidden = true; opts.undo(); } }) : null);
  el.hidden = false;
  clearTimeout(toastTimer);
  toastTimer = setTimeout(() => { el.hidden = true; }, opts.undo ? 6000 : 4000);
}

// ---------- Discover ----------

async function renderWall(seq) {
  view.replaceChildren(...header('Discover', 'New to you, because of what you\'ve watched and requested.'), gridSkeleton(18));
  let data;
  try { data = await api('GET', '/api/wall'); } catch (err) { if (seq === renderSeq) showNote('Could not load suggestions', err.message); return; }
  if (seq !== renderSeq) return;
  if (data.syncing) {
    setTimeout(() => { if (seq === renderSeq) render(); }, 3000);
    return;
  }
  // Blocked has no tab; this is the way to it, where blocking happens.
  const foot = data.blocked
    ? h('p', { class: 'foot' }, `${data.blocked} blocked title${data.blocked === 1 ? '' : 's'} · `, h('a', { href: '/blocked', text: 'Manage' }))
    : null;
  if (!data.cards.length) {
    return view.replaceChildren(...header('Discover'), h('div', { class: 'note' },
      h('strong', { text: data.seeds ? 'Nothing new to suggest right now' : 'Nothing to go on yet' }),
      data.seeds ? 'Everything recommended is already in the library, watched or blocked.'
        : 'Once you\'ve watched or requested something, suggestions show up here.'), foot);
  }
  const grid = h('div', { class: 'grid' });
  for (const c of data.cards) {
    const b = c.because;
    const card = h('a', { class: 'card', href: titleHref(c, [keyOf(b)]) },
      posterEl(c),
      h('div', { class: 'name', text: c.title }),
      h('div', { class: 'because' }, `Because you ${b.reason} `, h('b', { text: b.title || 'something' })));
    pressable(card, c, [blockItem(c, card)]);
    grid.append(card);
  }
  view.replaceChildren(...header('Discover', 'New to you, because of what you\'ve watched and requested.'), grid, foot);
}

// ---------- Title ----------

function titleSkeleton() {
  return h('div', {},
    h('div', { class: 'skel line short', style: 'width:220px;margin-bottom:16px' }),
    h('div', { class: 'hero' }, h('div', { class: 'hero-in' },
      h('div', { class: 'poster skel' }),
      h('div', { class: 'hero-text', style: 'flex:1' },
        h('div', { class: 'skel h1' }), h('div', { class: 'skel para', style: 'width:40%' }),
        h('div', { class: 'skel para' }), h('div', { class: 'skel para' }), h('div', { class: 'skel para', style: 'width:70%' })))),
    h('h2', { text: 'More like this' }), gridSkeleton(12));
}

// Where a collection part stands for you (server: partState).
const PART_STATES = {
  watched: ['watched', 'Watched'],
  library: ['library', 'In library'],
  requested: ['requested', 'Requested'],
};

function statusLabel(d) {
  switch (d.title.status) {
    case 5: return 'In the library';
    case 4: return 'Partly in the library';
    case 2: case 3: return 'Requested';
    case 6: return 'Blocklisted in Seerr';
  }
  return d.requested ? 'Requested' : null;
}

async function renderTitle(seq, type, id, viaRaw) {
  view.replaceChildren(titleSkeleton());
  let d;
  try {
    d = await api('GET', `/api/title/${type}/${id}` + (viaRaw ? `?via=${encodeURIComponent(viaRaw)}` : ''));
  } catch (err) { if (seq === renderSeq) showNote('Could not load this title', err.message); return; }
  if (seq !== renderSeq) return;
  const t = d.title;
  const via = d.trail.map(keyOf);

  // The path here, folded into a dropdown: closed it names the step you came
  // from; open it lists every step from Discover down to this title.
  const prev = d.trail.length ? d.trail[d.trail.length - 1].title || '…' : 'Discover';
  const steps = d.trail.length + 1; // the trail plus this title
  const crumbs = h('details', { class: 'trail' },
    h('summary', { 'aria-label': 'How you got here' },
      svg('<svg class="trail-icon" viewBox="0 0 24 24" aria-hidden="true"><circle cx="5" cy="18" r="2"/><circle cx="19" cy="6" r="2"/><path d="M7 18h4a4 4 0 0 0 4-4 4 4 0 0 1 4-4"/></svg>'),
      h('span', { class: 'trail-prev', text: prev }),
      h('span', { class: 'trail-count', text: `${steps} step${steps === 1 ? '' : 's'}` }),
      h('span', { class: 'trail-caret', 'aria-hidden': 'true', text: '▾' })),
    h('ol', {},
      h('li', {}, h('a', { href: '/', text: 'Discover' })),
      ...d.trail.map((s, i) => h('li', {}, h('a', { href: titleHref(s, via.slice(0, i)), text: s.title || '…' }))),
      h('li', { class: 'here', 'aria-current': 'page' }, h('span', { text: t.title }))));

  const facts = [t.year, t.runtime ? `${Math.floor(t.runtime / 60) ? Math.floor(t.runtime / 60) + 'h ' : ''}${t.runtime % 60}m` : null,
    t.seasons ? `${t.seasons} season${t.seasons === 1 ? '' : 's'}` : null,
    t.rating ? `★ ${t.rating.toFixed(1)}` : null, (t.genres || []).slice(0, 3).join(', ') || null,
    d.watched ? 'You\'ve watched this' : null].filter(Boolean);

  const actions = h('div', { class: 'actions' });
  const label = statusLabel(d);
  if (d.requestable) {
    const word = t.type === 'tv' ? 'Request series' : 'Request';
    const btn = h('button', { class: 'btn', text: word });
    // Asked up front so the popup opens at once; people Seerr doesn't let
    // choose (or a failed lookup) get no popup, just the request.
    const options = api('GET', `/api/request-options/${t.type}`).catch(() => ({ advanced: false }));
    const send = async (choice) => {
      btn.disabled = true; btn.textContent = 'Requesting…';
      try {
        const res = await api('POST', '/api/request', { type: t.type, id: t.id, via, ...choice });
        btn.textContent = res.dryRun ? 'Requested (dry run)' : 'Requested'; btn.classList.add('done');
        if (res.dryRun) toast('Dry run: nothing was sent to Seerr');
        return true;
      } catch (err) {
        btn.disabled = false; btn.textContent = word;
        toast(err.message, { error: true });
        return false;
      }
    };
    btn.addEventListener('click', async () => {
      const res = await options;
      if (seq !== renderSeq) return;
      if (res.advanced) requestDialog(t, res.options, send);
      else send({});
    });
    actions.append(btn);
  } else if (label) {
    actions.append(h('button', { class: 'btn done', disabled: true, text: label }));
  }
  const links = d.links || {};
  if (links.seerr) {
    actions.append(h('a', { class: 'btn ghost', href: links.seerr, target: '_blank', rel: 'noopener', text: 'Open in Seerr ↗' }));
  }
  // The collection's row, when it has more than this one part. Unlike the
  // rest of spor it shows parts you have or have seen: it's the whole series.
  const parts = d.collection || [];
  let partsRow = null;
  if (t.collection && parts.length > 1) {
    const next = [...via, keyOf(t)];
    const grid = h('div', { class: 'grid' });
    for (const p of parts) {
      const poster = posterEl(p);
      const state = p.current ? ['here', 'You\'re here'] : PART_STATES[p.state];
      if (state) poster.append(h('span', { class: `state ${state[0]}`, text: state[1] }));
      const kids = [poster, h('div', { class: 'name', text: p.title }), h('div', { class: 'meta', text: p.year || 'Coming' })];
      grid.append(p.current
        ? h('div', { class: 'card current' }, ...kids)
        : h('a', { class: 'card', href: titleHref(p, next) }, ...kids));
    }
    partsRow = h('section', { class: 'parts' }, h('h2', { text: t.collection.name }), grid);
  }
  const collection = t.collection
    ? h('p', { class: 'collection' }, 'Part of ', partsRow
      ? h('button', { class: 'linkish', type: 'button', text: t.collection.name,
        onclick: () => partsRow.scrollIntoView({ behavior: 'smooth', block: 'start' }) })
      : h('b', { text: t.collection.name }))
    : null;

  const hero = h('section', { class: 'hero' },
    t.backdrop ? h('div', { class: 'backdrop', style: `background-image:url("${IMG}w1280${t.backdrop}")` }) : null,
    h('div', { class: 'hero-in' }, posterEl(t),
      h('div', { class: 'hero-text' },
        h('h1', { text: t.title }),
        h('div', { class: 'facts' }, ...facts.map((f) => h('span', { text: f }))),
        collection,
        t.tagline ? h('p', { class: 'tagline', text: t.tagline }) : null,
        h('p', { class: 'overview', text: t.overview || 'No summary yet.' }),
        actions)));

  const more = [h('h2', { text: 'More like this' })];
  if (!d.recommendations.length) {
    more.push(h('p', { class: 'lede', text: 'No new recommendations for this one: everything is in the library, watched or blocked.' }));
  } else {
    const grid = h('div', { class: 'grid' });
    const next = [...via, keyOf(t)];
    for (const r of d.recommendations) {
      const card = h('a', { class: 'card', href: titleHref(r, next) },
        posterEl(r), h('div', { class: 'name', text: r.title }), h('div', { class: 'meta', text: r.year || '' }));
      pressable(card, r, [blockItem(r, card)]);
      grid.append(card);
    }
    more.push(grid);
  }
  view.replaceChildren(crumbs, hero, partsRow, ...more);
  document.title = `${t.title} · spor`;
}

// requestDialog is the advanced request, as in Seerr: a popup with the
// quality profile and root folder, pre-set to the server's defaults. A field
// left on its default isn't sent, so Seerr decides as for a plain request.
function requestDialog(t, o, send) {
  const choice = {};
  const select = (label, items, onPick) => {
    const s = h('select', {}, ...items.map(([value, text, isDefault]) =>
      h('option', { value, selected: isDefault, text: isDefault ? `${text} (default)` : text })));
    s.addEventListener('change', () => onPick(s.value));
    return h('label', {}, h('span', { text: label }), s);
  };
  const go = h('button', { class: 'btn', type: 'button', text: 'Request' });
  const dlg = h('dialog', { class: 'dialog', 'aria-label': `Request ${t.title}` },
    h('div', { class: 'dialog-head' },
      t.poster ? h('img', { src: IMG + 'w154' + t.poster, alt: '' }) : null,
      h('div', {}, h('div', { class: 'dialog-kicker', text: t.type === 'tv' ? 'Request series' : 'Request movie' }),
        h('div', { class: 'dialog-title', text: t.title }))),
    h('div', { class: 'dialog-body' },
      select('Quality profile', o.profiles.map((p) => [p.id, p.name, p.id === o.defaultProfile]), (v) => {
        if (Number(v) === o.defaultProfile) delete choice.profileId; else choice.profileId = Number(v);
      }),
      select('Root folder', o.rootFolders.map((f) => [f, f, f === o.defaultFolder]), (v) => {
        if (v === o.defaultFolder) delete choice.rootFolder; else choice.rootFolder = v;
      })),
    h('div', { class: 'dialog-foot' },
      h('button', { class: 'btn ghost', type: 'button', text: 'Cancel', onclick: () => dlg.close() }), go));
  go.addEventListener('click', async () => {
    go.disabled = true;
    if (await send(choice)) dlg.close();
    else go.disabled = false;
  });
  // A tap on the dimmed backdrop (the dialog itself, outside its content) cancels.
  dlg.addEventListener('click', (e) => { if (e.target === dlg) dlg.close(); });
  dlg.addEventListener('close', () => dlg.remove());
  document.body.append(dlg);
  dlg.showModal();
}

// ---------- Library ----------

async function renderLibrary(seq) {
  const lede = h('span', {}, 'Rate what you\'ve watched: ', thumb('up'), ' brings more like it, ', thumb('down'), ' fewer.');
  view.replaceChildren(...header('Library', lede), gridSkeleton(18, true));
  let data;
  try { data = await api('GET', '/api/library'); } catch (err) { if (seq === renderSeq) showNote('Could not load the library', err.message); return; }
  if (seq !== renderSeq) return;
  if (data.syncing) {
    setTimeout(() => { if (seq === renderSeq) render(); }, 3000);
    return;
  }
  // One section per group, each under a header that sticks below the title
  // bar while its section scrolls past (like the letters in iOS Contacts).
  // The chips above are shortcuts that scroll to a section, not filters.
  const groups = [
    ['Watched', data.items.filter((it) => it.watched)],
    ['Not watched', data.items.filter((it) => !it.watched)],
    // Watched, but no longer in the library (reaped, or from before spor):
    // still worth rating, since it shapes Discover. Name to be settled.
    ['Watched earlier', data.earlier || []],
  ].filter(([, items]) => items.length);
  const sections = groups.map(([label, items]) => h('section', { class: 'lib-section' },
    h('h2', { class: 'section-head' }, h('span', { text: label }), h('span', { class: 'count', text: String(items.length) })),
    h('div', { class: 'grid' }, ...items.map(libraryCard))));
  const jumps = h('div', { class: 'filters' }, ...groups.map(([label], i) =>
    h('button', { class: 'chip', type: 'button', text: label,
      onclick: () => sections[i].scrollIntoView({ behavior: 'smooth', block: 'start' }) })));
  view.replaceChildren(...header('Library', lede), jumps, ...sections);
}

// thumb is a thumbs-up or -down line icon (Lucide, ISC licence).
function thumb(dir) {
  const paths = dir === 'up'
    ? '<path d="M7 10v12"/><path d="M15 5.88 14 10h5.83a2 2 0 0 1 1.92 2.56l-2.33 8A2 2 0 0 1 17.5 22H4a2 2 0 0 1-2-2v-8a2 2 0 0 1 2-2h2.76a2 2 0 0 0 1.79-1.11L12 2a3.13 3.13 0 0 1 3 3.88Z"/>'
    : '<path d="M17 14V2"/><path d="M9 18.12 10 14H4.17a2 2 0 0 1-1.92-2.56l2.33-8A2 2 0 0 1 6.5 2H20a2 2 0 0 1 2 2v8a2 2 0 0 1-2 2h-2.76a2 2 0 0 0-1.79 1.11L12 22a3.13 3.13 0 0 1-3-3.88Z"/>';
  return svg(`<svg class="thumb" viewBox="0 0 24 24" aria-hidden="true">${paths}</svg>`);
}

function libraryCard(it) {
  const poster = posterEl(it);
  const card = h('div', { class: 'card' + (it.watched ? '' : ' unwatched') }, poster,
    h('div', { class: 'name', text: it.title }), h('div', { class: 'meta', text: it.year || '' }));
  if (!it.watched) {
    poster.append(h('div', { class: 'banner', text: 'Not watched' }));
    return card;
  }
  const up = h('button', { class: 'up', 'aria-label': `Liked ${it.title}` }, thumb('up'));
  const down = h('button', { class: 'down', 'aria-label': `Disliked ${it.title}` }, thumb('down'));
  const paint = () => {
    up.classList.toggle('on', it.rating === 1); up.setAttribute('aria-pressed', it.rating === 1);
    down.classList.toggle('on', it.rating === -1); down.setAttribute('aria-pressed', it.rating === -1);
  };
  const set = async (value) => {
    const before = it.rating;
    it.rating = it.rating === value ? 0 : value; // pressing the lit one clears it
    paint();
    try { await api('POST', '/api/rating', { type: it.type, id: it.id, value: it.rating }); } catch (err) {
      it.rating = before; paint(); toast(err.message, { error: true });
    }
  };
  up.addEventListener('click', () => set(1));
  down.addEventListener('click', () => set(-1));
  paint();
  card.append(h('div', { class: 'rate' }, up, down));
  return card;
}

// ---------- Blocked ----------

async function renderBlocked(seq) {
  const lede = 'Titles you said you\'re not interested in. They never show up in Discover.';
  const back = () => h('nav', { class: 'crumbs' }, h('a', { href: '/', text: '‹ Discover' }));
  view.replaceChildren(back(), ...header('Blocked', lede), gridSkeleton(12, true));
  let data;
  try { data = await api('GET', '/api/blocked'); } catch (err) { if (seq === renderSeq) showNote('Could not load blocked titles', err.message); return; }
  if (seq !== renderSeq) return;
  if (!data.items.length) {
    return view.replaceChildren(back(), ...header('Blocked'), h('div', { class: 'note' },
      h('strong', { text: 'Nothing blocked' }), 'Long-press (or right-click) a poster in Discover to block it.'));
  }
  const grid = h('div', { class: 'grid' });
  for (const b of data.items) {
    const btn = h('button', { class: 'btn ghost', style: 'width:100%;margin-top:8px;padding:6px 0', text: 'Unblock' });
    const card = h('div', { class: 'card' }, posterEl(b), h('div', { class: 'name', text: b.title || `${b.type} ${b.id}` }), btn);
    btn.addEventListener('click', async () => {
      btn.disabled = true;
      try { await api('POST', '/api/unblock', { type: b.type, id: b.id }); card.remove(); toast(`Unblocked ${b.title}`); } catch (err) {
        btn.disabled = false; toast(err.message, { error: true });
      }
    });
    grid.append(card);
  }
  view.replaceChildren(back(), ...header('Blocked', lede), grid);
}

render();
