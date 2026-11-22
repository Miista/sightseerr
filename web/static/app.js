'use strict';

// Four views on one page: Discover (the wall), a title's page (with the
// trail that led there), Library (rate what you've watched) and Blocked.
// The server decides who you are and checks everything again; what happens
// here is only for the person.

const $ = (id) => document.getElementById(id);
const IMG = '/img/'; // TMDb images, served (and kept) by spor: the page makes no outside requests
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
// Where you are counts as Discover on the wall, and on a title page unless
// the chain started in Library (root=library), which keeps Library's tab lit.
const isTitle = (path) => /^\/(movie|tv)\/\d+$/.test(path);
const fromLibrary = () => new URLSearchParams(location.search).get('root') === 'library';
const inDiscover = (path) => path === '/' || (isTitle(path) && !fromLibrary());

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
  // A title page's Back out to Discover or Library lands where you were there.
  go(url.pathname + url.search, { restore: a.dataset.restore === '1' });
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
  placeSliders(slidersPlaced);
  slidersPlaced = true;
}

// placeSliders moves each tab bar's coral pill to its active tab: behind the
// whole tab on desktop, behind the icon on a phone. It slides when animate
// is set, and jumps on first load and resize. No active tab: it fades out.
let slidersPlaced = false;
function placeSliders(animate) {
  for (const nav of document.querySelectorAll('.tabs, .bottom-bar')) {
    const on = nav.querySelector('a.on');
    slideTo(nav, on && (nav.classList.contains('bottom-bar') ? on.querySelector('.tab-icon') : on), animate);
  }
}

// slideTo moves the coral pill inside container to sit behind target (made
// on first use); no target fades it out. Also used by Library's switcher.
function slideTo(container, target, animate) {
  let slider = container.querySelector(':scope > .tab-slider');
  if (!slider) {
    slider = h('span', { class: 'tab-slider', 'aria-hidden': 'true' });
    container.prepend(slider);
  }
  if (!target) { slider.style.opacity = '0'; return; }
  const box = container.getBoundingClientRect(), r = target.getBoundingClientRect();
  if (!r.width) return; // hidden at this width
  slider.classList.toggle('instant', !animate || slider.style.opacity !== '1');
  slider.style.width = `${r.width}px`;
  slider.style.height = `${r.height}px`;
  slider.style.transform = `translate(${r.left - box.left - container.clientLeft}px, ${r.top - box.top - container.clientTop}px)`;
  slider.style.opacity = '1';
}
window.addEventListener('resize', () => placeSliders(false));
document.fonts?.ready.then(() => placeSliders(false)); // label widths change once the font is in

async function render() {
  const seq = ++renderSeq;
  for (const dlg of document.querySelectorAll('dialog[open]')) dlg.close();
  document.title = 'spor';
  setHeadTitle(null);
  undock();
  if (!isTitle(location.pathname)) closePanel(); // the path panel belongs to title pages
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
  // A title page belongs to the tab its chain started from.
  if (m) { setTab(fromLibrary() ? 'library' : 'wall'); return renderTitle(seq, m[1], Number(m[2]), new URLSearchParams(location.search)); }
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
      h('div', { class: 'card-text' }, h('div', { class: 'skel line' }), h('div', { class: 'skel line short' })),
      extra ? h('div', { class: 'skel line', style: 'height:42px;width:100%;border-radius:14px' }) : null));
  }
  return g;
}

// The small mono label above each page's title.
const KICKERS = { Discover: 'Fresh for you', Library: 'Your shelf', Blocked: 'Not interested' };

function header(title, lede) {
  return [h('div', { class: 'page-head' },
    KICKERS[title] ? h('div', { class: 'kicker', text: KICKERS[title] }) : null,
    h('h1', { text: title }),
    lede ? h('p', { class: 'lede' }, lede) : null)];
}

// cardText is a card's words under the poster: the title, then one line.
function cardText(title, line) {
  return h('div', { class: 'card-text' }, h('div', { class: 'name', text: title }), line);
}

// ---------- Block (press-and-hold on touch, right-click elsewhere) ----------

// An open path dropdown closes on a tap anywhere else.
document.addEventListener('pointerdown', (e) => {
  for (const w of document.querySelectorAll('.path-wrap')) {
    const drop = w.querySelector('.path-drop');
    if (drop && !drop.hidden && !w.contains(e.target)) {
      drop.hidden = true;
      w.querySelector('.path-pill')?.classList.remove('open');
    }
  }
});

// pressable calls onHold on a press-and-hold or a right-click, and swallows
// the click that ends a hold so it doesn't also open the title.
function pressable(el, t, onHold) {
  let timer = null, startX = 0, startY = 0, fired = false, lastOpen = 0;
  const open = () => {
    if (Date.now() - lastOpen < 600) return; // a touch hold can also send contextmenu
    lastOpen = Date.now();
    fired = true;
    onHold();
  };
  el.addEventListener('pointerdown', (e) => {
    fired = false;
    if (e.pointerType === 'mouse') return;
    startX = e.clientX; startY = e.clientY;
    timer = setTimeout(() => { navigator.vibrate?.(10); open(); }, 480);
  });
  el.addEventListener('pointermove', (e) => {
    if (timer && Math.hypot(e.clientX - startX, e.clientY - startY) > 10) { clearTimeout(timer); timer = null; }
  });
  for (const ev of ['pointerup', 'pointercancel', 'pointerleave']) el.addEventListener(ev, () => { clearTimeout(timer); timer = null; });
  el.addEventListener('contextmenu', (e) => { e.preventDefault(); open(); });
  el.addEventListener('click', (e) => { if (fired) { e.preventDefault(); e.stopPropagation(); fired = false; } }, true);
}

// blockSheet offers to block a title. Blocking hides the card at once; Undo
// puts it back where it was. (No "view details": a normal tap opens it.)
function blockSheet(t, card) {
  const block = async () => {
    dlg.close();
    const parent = card.parentNode, next = card.nextSibling;
    card.remove();
    try {
      await api('POST', '/api/block', { type: t.type, id: t.id });
    } catch (err) {
      parent?.insertBefore(card, next);
      return toast(err.message, { error: true });
    }
    toast(`${t.title} blocked`, {
      undo: async () => {
        try {
          await api('POST', '/api/unblock', { type: t.type, id: t.id });
          parent?.insertBefore(card, next);
        } catch (err) { toast(err.message, { error: true }); }
      },
    });
  };
  const dlg = sheet(`Block ${t.title}`,
    h('div', { class: 'dialog-head' },
      t.poster ? h('img', { src: IMG + 'w154' + t.poster, alt: '' }) : null,
      h('div', { class: 'dialog-title', text: t.title })),
    h('div', { class: 'dialog-body' },
      h('button', { class: 'block-btn', type: 'button', onclick: block },
        h('span', { class: 'block-label', text: 'Not interested — block' }),
        h('span', { class: 'block-sub', text: 'Hides it and steers recommendations away' }))),
    h('div', { class: 'dialog-foot' }, h('button', { class: 'btn ghost', type: 'button', text: 'Cancel', onclick: () => dlg.close() })));
}

// ---------- Toast ----------

let toastTimer = null;
function toast(msg, opts = {}) {
  const el = $('toast');
  el.classList.toggle('error', !!opts.error);
  el.replaceChildren(...[h('span', { text: msg }),
    opts.undo ? h('button', { text: 'Undo', onclick: () => { el.hidden = true; opts.undo(); } }) : null].filter(Boolean));
  el.hidden = false;
  clearTimeout(toastTimer);
  toastTimer = setTimeout(() => { el.hidden = true; }, opts.undo ? 4500 : 4000);
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
        : 'Once you\'ve watched or requested something, suggestions show up here.'), ...(foot ? [foot] : []));
  }
  const grid = h('div', { class: 'grid' });
  for (const c of data.cards) {
    const b = c.because;
    const card = h('a', { class: 'card', href: titlePath(c, { seed: keyOf(b) }) },
      posterEl(c),
      cardText(c.title, h('div', { class: 'because' }, `Because you ${b.reason} `, h('b', { text: b.title || 'something' }))));
    pressable(card, c, () => blockSheet(c, card));
    grid.append(card);
  }
  view.replaceChildren(...header('Discover', 'New to you, because of what you\'ve watched and requested.'), grid, ...(foot ? [foot] : []));
}

// ---------- Title ----------

// A title's path: the titles opened before it (via) and, after jumping back,
// the ones opened after it (ahead), plus where the chain started (root:
// Discover or Library). It all lives in the URL, so Back, reload and shared
// links keep it. seed (straight from Discover) is only for spor's own
// record of what led where; it isn't part of the path you see.
function titlePath(t, { via = [], ahead = [], root = '', seed = '' } = {}) {
  const q = [];
  if (via.length) q.push('via=' + via.join(','));
  if (ahead.length) q.push('ahead=' + ahead.join(','));
  if (root === 'library') q.push('root=library');
  if (seed) q.push('seed=' + seed);
  return `/${t.type}/${t.id}` + (q.length ? '?' + q.join('&') : '');
}
const rootPath = (root) => (root === 'library' ? '/library' : '/');
const wide = () => matchMedia('(min-width: 821px)').matches;
// Phone and desktop lay the title page out differently; crossing the
// breakpoint redraws it (the path and everything else are in the URL).
matchMedia('(min-width: 821px)').addEventListener('change', () => { if (/^\/(movie|tv)\//.test(location.pathname)) render(); });

const ICONS = {
  path: '<svg class="ico path" viewBox="0 0 24 24" aria-hidden="true"><circle cx="6" cy="19" r="2.5"/><path d="M9 19h8.5a3.5 3.5 0 0 0 0-7h-11a3.5 3.5 0 0 1 0-7H15"/><circle cx="18" cy="5" r="2.5"/></svg>',
  back: '<svg class="ico" viewBox="0 0 24 24" aria-hidden="true"><path d="m15 18-6-6 6-6"/></svg>',
  down: '<svg class="ico chev" viewBox="0 0 24 24" aria-hidden="true"><path d="m6 9 6 6 6-6"/></svg>',
  // The desktop pill's: points right, and left while the panel is open.
  side: '<svg class="ico chev" viewBox="0 0 24 24" aria-hidden="true"><path d="m9 18 6-6-6-6"/></svg>',
  right: '<svg class="ico" viewBox="0 0 24 24" aria-hidden="true"><path d="m9 18 6-6-6-6"/></svg>',
  x: '<svg class="ico" viewBox="0 0 24 24" aria-hidden="true"><path d="M18 6 6 18"/><path d="m6 6 12 12"/></svg>',
};

function titleSkeleton() {
  return h('div', { class: 'detail', 'aria-busy': 'true' },
    h('div', { class: 'd-head d-skel' },
      h('div', { class: 'd-thumb skel' }),
      h('div', { class: 'd-title' }, h('div', { class: 'skel h1' }), h('div', { class: 'skel para', style: 'width:60%' }))),
    h('div', { class: 'd-info' },
      h('div', { class: 'skel para' }), h('div', { class: 'skel para' }), h('div', { class: 'skel para', style: 'width:80%' })));
}

// Where a collection part stands for you (server: partState).
const PART_STATES = { watched: 'Watched', library: 'In library', requested: 'Requested' };

function statusLabel(d) {
  switch (d.title.status) {
    case 5: return 'In the library';
    case 4: return 'Partly in the library';
    case 2: case 3: return 'Requested ✓';
    case 6: return 'Blocklisted in Seerr';
  }
  return d.requested ? 'Requested ✓' : null;
}

// statsBlock: year, runtime (or seasons) and genre, then the outside scores
// with their real logos (as Seerr shows them, from Seerr's own assets).
function statsBlock(t, s = {}) {
  const runtime = t.runtime ? `${Math.floor(t.runtime / 60) ? Math.floor(t.runtime / 60) + 'h ' : ''}${t.runtime % 60}m` : null;
  const facts = [['Year', t.year],
    t.type === 'tv' ? ['Seasons', t.seasons ? String(t.seasons) : null] : ['Runtime', runtime],
    ['Genre', (t.genres || []).slice(0, 3).join(', ')]];
  const top = h('div', { class: 'stat-row' }, ...facts.map(([label, value], i) =>
    h('div', { class: 'stat' + (i === 2 ? ' wide' : '') }, h('div', { class: 'stat-label', text: label }), h('div', { class: 'stat-value', text: value || '—' }))));
  const scores = scoreItems(t, s);
  return h('div', { class: 'stats' }, top, scores.length ? h('div', { class: 'score-row' }, ...scores) : null);
}

// scoresRow is the scores alone, between thin lines (the phone's layout).
function scoresRow(t, s = {}) {
  const scores = scoreItems(t, s);
  return scores.length ? h('div', { class: 'stats' }, h('div', { class: 'score-row' }, ...scores)) : null;
}

function scoreItems(t, s = {}) {
  const score = (logo, alt, value, href) => {
    const kids = [h('img', { src: `/vendor/ratings/${logo}.svg`, alt, class: logo === 'imdb' || logo === 'tmdb_logo' ? 'logo-wide' : '' }), h('span', { text: value })];
    return href ? h('a', { class: 'score', href, target: '_blank', rel: 'noopener', title: alt }, ...kids) : h('div', { class: 'score', title: alt }, ...kids);
  };
  return [
    s.rtCritics ? score(s.rtCriticsRotten ? 'rt_rotten' : 'rt_fresh', 'Rotten Tomatoes critics', `${s.rtCritics}%`, s.rtUrl) : null,
    s.rtAudience ? score(s.rtAudienceRotten ? 'rt_aud_rotten' : 'rt_aud_fresh', 'Rotten Tomatoes audience', `${s.rtAudience}%`, s.rtUrl) : null,
    s.imdb ? score('imdb', 'IMDb', s.imdb.toFixed(1), s.imdbUrl) : null,
    t.rating ? score('tmdb_logo', 'TMDB user score', `${Math.round(t.rating * 10)}%`, `https://www.themoviedb.org/${t.type}/${t.id}`) : null,
  ].filter(Boolean);
}

// pathList: the titles of this chain, from the first one opened down to this
// one, then any you jumped back from (faded, so you can go forward again).
function pathList(d, root) {
  const t = d.title, cur = keyOf(t);
  const trail = d.trail.map(keyOf), ahead = (d.ahead || []).map(keyOf);
  if (!trail.length && !ahead.length) {
    return h('p', { class: 'path-empty', text: 'Titles you open from here get added to your path, so you can jump back to any of them.' });
  }
  return h('ol', { class: 'plist' },
    ...d.trail.map((s, i) => h('li', {}, h('a', { href: titlePath(s, { via: trail.slice(0, i), ahead: [...trail.slice(i + 1), cur, ...ahead], root }), text: s.title || '…' }))),
    h('li', { class: 'here', 'aria-current': 'page' }, h('span', { text: t.title })),
    ...(d.ahead || []).map((s, j) => h('li', { class: 'ahead' }, h('a', { href: titlePath(s, { via: [...trail, cur, ...ahead.slice(0, j)], ahead: ahead.slice(j + 1), root }), text: s.title || '…' }))));
}

// centre keeps the current title in view inside a scrolling path list.
function centre(list) {
  const cur = list?.querySelector('.here');
  if (cur) list.scrollTop = cur.offsetTop - list.clientHeight / 2 + cur.offsetHeight / 2;
}

// The desktop path panel stays open across titles until closed.
const panelWanted = () => sessionStorage.getItem('spor.panel') === '1';
function closePanel() {
  document.querySelector('.path-panel')?.remove();
  document.body.classList.remove('panel-open');
}
function showPanel(d, root, animate) {
  closePanel();
  const panel = h('aside', { class: 'path-panel', 'aria-label': 'Path' },
    h('div', { class: 'panel-head' }, h('span', { class: 'kicker', text: 'How you got here' }),
      h('button', { class: 'icon-btn', type: 'button', 'aria-label': 'Close the path',
        onclick: () => { sessionStorage.setItem('spor.panel', '0'); closePanel(); document.querySelector('.path-pill')?.classList.remove('on'); } }, svg(ICONS.x))),
    pathList(d, root));
  document.body.append(panel);
  document.body.classList.add('panel-open');
  requestAnimationFrame(() => { panel.classList.toggle('open', true); if (!animate) panel.classList.add('instant'); centre(panel.querySelector('.plist')); });
}

async function renderTitle(seq, type, id, q) {
  const root = q.get('root') === 'library' ? 'library' : '';
  view.replaceChildren(titleSkeleton());
  const ask = new URLSearchParams();
  for (const k of ['via', 'ahead', 'seed']) if (q.get(k)) ask.set(k, q.get(k));
  let d;
  try { d = await api('GET', `/api/title/${type}/${id}?${ask}`); } catch (err) { if (seq === renderSeq) showNote('Could not load this title', err.message); return; }
  if (seq !== renderSeq) return;
  const t = d.title, cur = keyOf(t);
  const trail = d.trail.map(keyOf), ahead = (d.ahead || []).map(keyOf);
  // Going on from this page: the path grows by this title; if the next title
  // is the first one ahead of you, the rest of what's ahead is kept.
  const onward = (x) => titlePath(x, { via: [...trail, cur], ahead: ahead[0] === keyOf(x) ? ahead.slice(1) : [], root });
  // Back: one title up the path (this one goes ahead), or out to where the
  // chain started. Opening from Discover and pressing Back is always Discover.
  const back = trail.length
    ? titlePath(d.trail[trail.length - 1], { via: trail.slice(0, -1), ahead: [cur, ...ahead], root })
    : rootPath(root);
  const isWide = wide();

  // The path pill: a dropdown on a phone, the side panel on desktop.
  const pill = h('button', { class: 'path-pill', type: 'button', 'aria-expanded': 'false' }, svg(ICONS.path), h('span', { text: 'Path' }), svg(isWide ? ICONS.side : ICONS.down));
  const pathWrap = h('div', { class: 'path-wrap' }, pill);
  if (isWide) {
    if (panelWanted()) { pill.classList.add('on'); showPanel(d, root, false); } else closePanel();
    pill.addEventListener('click', () => {
      const open = !document.querySelector('.path-panel');
      sessionStorage.setItem('spor.panel', open ? '1' : '0');
      pill.classList.toggle('on', open);
      if (open) showPanel(d, root, true); else closePanel();
    });
  } else {
    closePanel();
    const drop = h('div', { class: 'path-drop', hidden: true }, h('div', { class: 'kicker', text: 'How you got here' }), pathList(d, root));
    pathWrap.append(drop);
    pill.addEventListener('click', () => {
      drop.hidden = !drop.hidden;
      pill.setAttribute('aria-expanded', String(!drop.hidden));
      pill.classList.toggle('open', !drop.hidden);
      if (!drop.hidden) centre(drop.querySelector('.plist'));
    });
  }
  const backBtn = isWide
    ? h('a', { class: 'pill-btn', href: back, 'data-restore': trail.length ? null : '1' }, svg(ICONS.back), 'Back')
    : h('a', { class: 'round-btn', href: back, 'aria-label': 'Back', 'data-restore': trail.length ? null : '1' }, svg(ICONS.back));

  // Request: full width on a phone; "Requested ✓" once asked for.
  const actions = h('div', { class: 'actions' });
  const label = statusLabel(d);
  if (d.requestable) {
    const word = t.type === 'tv' ? 'Request series' : 'Request';
    const btn = h('button', { class: 'btn req', text: word });
    // Asked up front so the sheet opens at once; people Seerr doesn't let
    // choose (or a failed lookup) get no sheet, just the request.
    const options = api('GET', `/api/request-options/${t.type}`).catch(() => ({ advanced: false }));
    const send = async (choice) => {
      btn.disabled = true; btn.textContent = 'Requesting…';
      try {
        const res = await api('POST', '/api/request', { type: t.type, id: t.id, via: trail, ...choice });
        btn.textContent = res.dryRun ? 'Requested ✓ (dry run)' : 'Requested ✓';
        btn.classList.add('done', 'pop');
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
    actions.append(h('button', { class: 'btn req done', disabled: true, text: label }));
  }
  if (d.watchlistKnown) actions.append(watchlistButton(t, d.watchlist));

  // The collection, collapsed under a pinned bar: every part in release
  // order, the one you're on marked "You're here" (and not a link).
  const parts = d.collection || [];
  const others = parts.filter((p) => !p.current);
  let coll = null;
  if (t.collection && others.length) {
    const list = h('div', { class: 'coll-list', hidden: true }, ...parts.map((p) => {
      const kids = [
        p.poster ? h('img', { src: IMG + 'w154' + p.poster, alt: '', loading: 'lazy' }) : h('div', { class: 'coll-noimg' }),
        h('div', { class: 'coll-text' },
          h('span', { class: `kind ${p.type}`, text: p.type === 'tv' ? 'Series' : 'Movie' }),
          h('div', { class: 'coll-name', text: p.title }),
          h('div', { class: 'coll-meta' }, h('span', { text: p.year || 'Coming' }),
            p.current ? h('span', { class: 'state here', text: 'You\'re here' })
              : PART_STATES[p.state] ? h('span', { class: `state ${p.state}`, text: PART_STATES[p.state] }) : null)),
      ];
      return p.current
        ? h('div', { class: 'coll-row current', 'aria-current': 'page' }, ...kids)
        : h('a', { class: 'coll-row', href: onward(p) }, ...kids, svg(ICONS.right));
    }));
    const bar = h('button', { class: 'coll-bar', type: 'button', 'aria-expanded': 'false' },
      h('div', {},
        h('div', { class: 'kicker', text: `Collection · ${others.length} more title${others.length === 1 ? '' : 's'}` }),
        h('div', { class: 'coll-title', text: t.collection.name })),
      h('span', { class: 'round-chev' }, svg(ICONS.down)));
    const setOpen = (open) => { list.hidden = !open; bar.setAttribute('aria-expanded', String(open)); bar.classList.toggle('open', open); };
    bar.addEventListener('click', () => setOpen(list.hidden));
    coll = h('section', { class: 'coll' }, bar, list);
    coll.openAndShow = () => { setOpen(true); coll.scrollIntoView({ behavior: 'smooth', block: 'start' }); };
  }
  const partOf = t.collection
    ? h('p', { class: 'part-of' }, 'Part of ', coll
      ? h('button', { class: 'linkish', type: 'button', text: t.collection.name, onclick: () => coll.openAndShow() })
      : h('b', { text: t.collection.name }))
    : null;
  const seerrLink = d.links?.seerr ? h('a', { class: 'seerr-link', href: d.links.seerr, target: '_blank', rel: 'noopener', text: 'Open in Seerr ↗' }) : null;

  const titleBlock = h('div', { class: 'd-title' }, badgeRow(t), h('h1', { text: t.title }));
  const words = [
    t.tagline ? h('p', { class: 'tagline', text: `“${t.tagline}”` }) : null,
    h('p', { class: 'overview', text: t.overview || 'No summary yet.' }),
  ];
  const posterImg = t.poster ? h('img', { src: IMG + 'w780' + t.poster, alt: '' }) : h('div', { class: 'noimg', text: t.title });

  let top;
  if (isWide) {
    top = [
      h('div', { class: 'detail-bar' }, backBtn, pathWrap),
      h('div', { class: 'detail-grid' },
        h('div', { class: 'd-poster' }, posterImg),
        h('div', { class: 'd-info' }, titleBlock, statsBlock(t, d.scores),
          h('div', { class: 'd-act' }, actions, partOf), ...words, seerrLink)),
    ];
  } else {
    // Phone: Back and Path in a row, a compact header (thumbnail beside the
    // title, year · runtime, genres), the scores, then tagline and summary
    // early, since that's what sells the recommendation, then Request.
    const runtime = t.type === 'tv'
      ? (t.seasons ? `${t.seasons} season${t.seasons === 1 ? '' : 's'}` : '')
      : (t.runtime ? `${Math.floor(t.runtime / 60) ? Math.floor(t.runtime / 60) + 'h ' : ''}${t.runtime % 60}m` : '');
    top = [
      h('div', { class: 'd-nav' }, backBtn, pathWrap),
      h('div', { class: 'd-head' },
        h('div', { class: 'd-thumb' }, t.poster ? h('img', { src: IMG + 'w342' + t.poster, alt: '' }) : null),
        h('div', { class: 'd-title' },
          badgeRow(t),
          h('h1', { text: t.title }),
          h('div', { class: 'd-year', text: [t.year, runtime].filter(Boolean).join(' · ') }),
          (t.genres || []).length ? h('div', { class: 'd-genres', text: t.genres.join(', ') }) : null)),
      h('div', { class: 'd-info' }, scoresRow(t, d.scores), ...words, actions, h('div', { class: 'd-links' }, partOf, seerrLink)),
    ];
  }

  const more = h('section', { class: 'more' }, h('div', { class: 'section-head' }, h('div', { class: 'title-row' }, h('h2', { text: 'More like this' }))));
  if (!d.recommendations.length) {
    more.append(h('p', { class: 'lede more-empty', text: 'No new recommendations for this one: everything is in the library, watched or blocked.' }));
  } else {
    const grid = h('div', { class: 'grid' });
    for (const r of d.recommendations) {
      const card = h('a', { class: 'card', href: onward(r) }, posterEl(r), cardText(r.title, h('div', { class: 'meta', text: r.year || '' })));
      pressable(card, r, () => blockSheet(r, card));
      grid.append(card);
    }
    more.append(grid);
  }
  // replaceChildren would print a missing section as the text "null".
  view.replaceChildren(...[h('div', { class: 'detail' }, ...top), coll, more].filter(Boolean));
  document.title = `${t.title} · spor`;
  setHeadTitle(t.title, view.querySelector('.d-title h1'));
  if (!isWide) dockRequest(actions.querySelector('.btn.req'));
}

// dockRequest: on a phone, while the Request button's place on the page is
// still below the screen, a docked copy sits just above the tab bar; once
// you've scrolled to the button itself, the copy goes. The copy mirrors the
// button (label, state) and hands taps to it, and hides while a sheet is open.
let dockWatch = null;
function undock() {
  document.querySelector('.req-dock')?.remove();
  window.removeEventListener('scroll', dockWatch);
  window.removeEventListener('resize', dockWatch);
  window.removeEventListener('spor:sheet', dockWatch);
  dockWatch = null;
}
function dockRequest(btn) {
  undock();
  if (!btn) return;
  const copy = h('button', { type: 'button' });
  const mirror = () => { copy.className = btn.className; copy.textContent = btn.textContent; copy.disabled = btn.disabled; };
  mirror();
  new MutationObserver(mirror).observe(btn, { attributes: true, childList: true, characterData: true, subtree: true });
  copy.addEventListener('click', () => btn.click());
  const dock = h('div', { class: 'req-dock', hidden: true }, copy);
  document.body.append(dock);
  const bar = document.querySelector('.bottom-bar');
  dockWatch = () => {
    if (!btn.isConnected) return undock();
    const limit = (bar ? bar.getBoundingClientRect().top : innerHeight) - 12;
    const docked = btn.getBoundingClientRect().bottom > limit && !document.querySelector('dialog[open]');
    dock.hidden = !docked;
    btn.style.visibility = docked ? 'hidden' : '';
  };
  window.addEventListener('scroll', dockWatch, { passive: true });
  window.addEventListener('resize', dockWatch);
  window.addEventListener('spor:sheet', dockWatch);
  dockWatch();
}

// Once the page's title has scrolled under the header, the header shows it
// next to the wordmark.
const headTitle = $('headTitle');
let headWatch = null;
function setHeadTitle(text, h1) {
  window.removeEventListener('scroll', headWatch);
  headTitle.classList.remove('show');
  headWatch = null;
  if (!text) return;
  headTitle.querySelector('.txt').textContent = text;
  headWatch = () => {
    if (!h1.isConnected) { window.removeEventListener('scroll', headWatch); return; }
    headTitle.classList.toggle('show', h1.getBoundingClientRect().bottom < topbar.offsetHeight);
  };
  window.addEventListener('scroll', headWatch, { passive: true });
  headWatch();
}

// watchlistButton puts the title on your Seerr watchlist, or takes it off:
// Seerr's star (Heroicons' outline star in amber), filled once it's on.
const STAR = '<svg viewBox="0 0 24 24" aria-hidden="true"><path d="M11.48 3.499a.562.562 0 0 1 1.04 0l2.125 5.111a.563.563 0 0 0 .475.345l5.518.442c.499.04.701.663.321.988l-4.204 3.602a.563.563 0 0 0-.182.557l1.285 5.385a.562.562 0 0 1-.84.61l-4.725-2.885a.562.562 0 0 0-.586 0L6.982 20.54a.562.562 0 0 1-.84-.61l1.285-5.386a.562.562 0 0 0-.182-.557l-4.204-3.602a.562.562 0 0 1 .321-.988l5.518-.442a.563.563 0 0 0 .475-.345L11.48 3.5Z"/></svg>';
function watchlistButton(t, on) {
  const btn = h('button', { class: 'btn ghost star-btn', type: 'button' }, svg(STAR));
  const paint = () => {
    btn.classList.toggle('on', on);
    btn.setAttribute('aria-pressed', String(on));
    btn.setAttribute('aria-label', on ? 'Remove from watchlist' : 'Add to watchlist');
    btn.title = on ? 'Remove from watchlist' : 'Add to watchlist';
  };
  paint();
  btn.addEventListener('click', async () => {
    btn.disabled = true;
    try {
      const res = await api('POST', '/api/watchlist', { type: t.type, id: t.id, on: !on });
      on = res.watchlist;
      paint();
      btn.classList.remove('pop'); void btn.offsetWidth; btn.classList.add('pop');
      toast(res.dryRun ? 'Dry run: the watchlist was not changed' : on ? `${t.title} added to your watchlist` : `${t.title} removed from your watchlist`);
    } catch (err) {
      toast(err.message, { error: true });
    } finally {
      btn.disabled = false;
    }
  });
  return btn;
}

// badgeRow is the MOVIE/SERIES badge with a small ⓘ beside it that explains
// the title's score ("why this score").
function badgeRow(t) {
  return h('div', { class: 'badge-row' },
    h('span', { class: `kind ${t.type}`, text: t.type === 'tv' ? 'Series' : 'Movie' }),
    h('button', { class: 'info-btn', type: 'button', 'aria-label': 'Why this is recommended', onclick: () => whySheet(t) },
      svg('<svg viewBox="0 0 24 24" aria-hidden="true"><circle cx="12" cy="12" r="10"/><path d="M12 16v-4"/><path d="M12 8h.01"/></svg>')));
}

const WHY_REASONS = { liked: 'You liked', watched: 'You watched', requested: 'You requested', disliked: 'You disliked' };
const WHY_HIDDEN = {
  library: 'It\'s already in the library, so Discover doesn\'t show it.',
  yours: 'You\'ve watched or requested it, so Discover doesn\'t show it.',
  blocked: 'You blocked it, so Discover doesn\'t show it.',
};
const ordinal = (n) => n + (n % 100 >= 11 && n % 100 <= 13 ? 'th' : ['th', 'st', 'nd', 'rd'][n % 10] || 'th');

// whySheet: which of your titles recommend this one, how each counts, and
// the score that results, worked out the same way as Discover.
async function whySheet(t) {
  const body = h('div', { class: 'dialog-body why-body' }, h('div', { class: 'skel para' }), h('div', { class: 'skel para', style: 'width:70%' }));
  const dlg = sheet(`Why ${t.title} is recommended`,
    h('div', { class: 'dialog-head' },
      h('div', {}, h('div', { class: 'dialog-kicker', text: 'Why this is recommended' }), h('div', { class: 'dialog-title', text: t.title }))),
    body,
    h('div', { class: 'dialog-foot' }, h('button', { class: 'btn ghost', type: 'button', text: 'Close', onclick: () => dlg.close() })));
  let d;
  try { d = await api('GET', `/api/why/${t.type}/${t.id}`); } catch (err) {
    body.replaceChildren(h('p', { class: 'why-note', text: err.message }));
    return;
  }
  const fmt = (n) => (n > 0 ? '+' : '') + (Math.round(n * 10) / 10);
  const kids = [];
  if (d.by.length) {
    kids.push(h('div', { class: 'why-score' }, h('span', { class: 'why-num', text: (Math.round(d.score * 10) / 10).toString() }),
      h('span', { class: 'why-of', text: `from ${d.by.length} of your title${d.by.length === 1 ? '' : 's'}` })));
    kids.push(h('ol', { class: 'why-list' }, ...d.by.map((c) => h('li', {},
      h('div', { class: 'why-main' },
        h('div', { class: 'why-title', text: c.title || '…' }),
        h('div', { class: 'why-sub', text: `${WHY_REASONS[c.reason] || ''} · ${ordinal(c.position)} of ${c.of} in TMDb's list for it` })),
      h('span', { class: 'why-weight' + (c.weight < 0 ? ' neg' : ''), text: fmt(c.weight) })))));
  } else {
    kids.push(h('p', { class: 'why-note', text: 'None of your titles recommend this one; you got here through the path.' }));
  }
  if (d.hidden) kids.push(h('p', { class: 'why-note', text: WHY_HIDDEN[d.hidden] }));
  if (d.failed) kids.push(h('p', { class: 'why-note', text: `${d.failed} of your titles couldn't be checked right now.` }));
  kids.push(h('p', { class: 'why-how', text: 'Each of your titles that recommends it adds to its score: liked +1.5, watched +1.0, requested +0.8, disliked −1.0. Discover shows the highest scores first.' }));
  body.replaceChildren(...kids);
}

// requestDialog is the advanced request, as in Seerr: a sheet with the
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
  const dlg = sheet(`Request ${t.title}`,
    h('div', { class: 'dialog-head' },
      t.poster ? h('img', { src: IMG + 'w154' + t.poster, alt: '' }) : null,
      h('div', {}, h('div', { class: 'dialog-kicker', text: t.type === 'tv' ? 'Request series' : 'Request movie' }),
        h('div', { class: 'dialog-title', text: t.title }),
        // A series is always requested in full.
        t.type === 'tv' ? h('div', { class: 'dialog-sub', text: 'All seasons' }) : null)),
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
}

// sheet opens a modal: a sheet rising from the bottom on a phone, a centred
// card on desktop. A tap on the dimmed backdrop (the dialog itself, outside
// its content) closes it; it removes itself once closed.
function sheet(label, ...content) {
  const dlg = h('dialog', { class: 'dialog', 'aria-label': label }, h('div', { class: 'sheet-grab', 'aria-hidden': 'true' }), ...content);
  dlg.addEventListener('click', (e) => { if (e.target === dlg) dlg.close(); });
  dlg.addEventListener('close', () => { dlg.remove(); window.dispatchEvent(new Event('spor:sheet')); });
  document.body.append(dlg);
  dlg.showModal();
  window.dispatchEvent(new Event('spor:sheet')); // the docked Request hides while a sheet is open
  return dlg;
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
  const groups = [
    ['Watched', null, data.items.filter((it) => it.watched)],
    ['Not watched', null, data.items.filter((it) => !it.watched)],
    // Watched, but no longer in the library (reaped, or from before spor):
    // still worth rating, since it shapes Discover.
    ['Removed', 'Watched, then deleted from the library. Ratings still count.',
      (data.earlier || []).map((it) => ({ ...it, removed: true }))],
  ].filter(([, , items]) => items.length);
  const sections = groups.map(([label, sub, items]) => h('section', { class: 'lib-section' },
    h('div', { class: 'section-head' },
      h('div', { class: 'title-row' }, h('h2', { text: label }), h('span', { class: 'count', text: String(items.length) })),
      sub ? h('span', { class: 'sub', text: sub }) : null),
    h('div', { class: 'grid' }, ...items.map(libraryCard))));
  // The switcher scrolls to a section rather than filtering, and its
  // highlight follows the scroll position.
  const buttons = groups.map(([label], i) => h('button', { type: 'button', text: label,
    onclick: () => sections[i].scrollIntoView({ behavior: 'smooth', block: 'start' }) }));
  const segments = h('div', { class: 'segments', role: 'tablist' }, ...buttons);
  // The coral pill behind the active segment slides along with it.
  let current = -1;
  const follow = (animate = true) => {
    if (!segments.isConnected) { window.removeEventListener('scroll', onScroll); window.removeEventListener('resize', snap); return; }
    const line = scrollY + topbar.offsetHeight + 80;
    const atEnd = scrollY + innerHeight >= document.documentElement.scrollHeight - 4;
    let on = 0;
    sections.forEach((s, i) => { if (s.offsetTop <= line) on = i; });
    if (atEnd) on = sections.length - 1;
    if (on === current && animate) return;
    current = on;
    buttons.forEach((b, i) => { b.classList.toggle('on', i === on); b.setAttribute('aria-selected', String(i === on)); });
    slideTo(segments, buttons[on], animate);
  };
  const snap = () => follow(false), onScroll = () => follow(true);
  window.addEventListener('scroll', onScroll, { passive: true });
  window.addEventListener('resize', snap);
  view.replaceChildren(...header('Library', lede), segments, ...sections);
  follow(false);
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
  const variant = it.removed ? ' removed' : it.watched ? '' : ' unwatched';
  // The poster opens the title; the path starts here, in Library.
  const card = h('div', { class: 'card' + variant },
    h('a', { class: 'poster-link', href: titlePath(it, { root: 'library' }), 'aria-label': it.title }, poster),
    cardText(it.title, h('div', { class: 'meta', text: it.year || '' })));
  if (!it.watched) {
    poster.append(h('div', { class: 'banner', text: 'NOT WATCHED' }));
    return card;
  }
  if (it.removed) {
    poster.append(h('span', { class: 'removed-tag' },
      svg('<svg viewBox="0 0 24 24" aria-hidden="true"><path d="M3 6h18"/><path d="M19 6v14a2 2 0 0 1-2 2H7a2 2 0 0 1-2-2V6"/><path d="M8 6V4a2 2 0 0 1 2-2h4a2 2 0 0 1 2 2v2"/></svg>'),
      'REMOVED'));
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
    const btn = h('button', { class: 'btn ghost', style: 'width:100%;height:42px;font-size:15px;border-radius:14px', text: 'Unblock' });
    const card = h('div', { class: 'card' }, posterEl(b), cardText(b.title || `${b.type} ${b.id}`, h('div', { class: 'meta', text: b.year || '' })), btn);
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
