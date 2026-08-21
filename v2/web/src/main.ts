import {
  createElement, type IconNode,
  Voicemail as VoicemailIcon, Phone, PhoneForwarded, Wrench, Play, Pause,
  Download, Check, Archive, Rocket, TriangleAlert, CircleCheck, Inbox, Trash2,
  ChevronRight, LogOut,
} from 'lucide';

function icon(node: IconNode, size = 16): SVGElement {
  const svg = createElement(node);
  svg.setAttribute('width', String(size));
  svg.setAttribute('height', String(size));
  svg.classList.add('icon');
  svg.setAttribute('aria-hidden', 'true');
  return svg;
}

interface Voicemail {
  id: number;
  received_at: string;
  subject: string;
  email_text: string;
  transcription: string;
  has_audio: boolean;
  done: boolean;
  done_at?: string;
}

type Filter = 'all' | 'open' | 'done';

interface AppEvent {
  id: number;
  at: string;
  kind: string;
  detail: string;
}

interface Action {
  name: string;
  kind: 'command' | 'storingsdienst';
  primary: boolean;
  group: string;
}

interface Status {
  uptime_seconds: number;
  voicemail_count: number;
  watcher: { last_poll: string; last_error: string };
  auth_enabled: boolean;
}

let voicemails: Voicemail[] = [];
let currentAudio: HTMLAudioElement | null = null;
let filter: Filter = 'all';

const $ = <T extends HTMLElement>(sel: string): T => {
  const el = document.querySelector<T>(sel);
  if (!el) throw new Error(`missing element ${sel}`);
  return el;
};

// A 401 means the login session expired: reload so the login screen appears.
function checkAuth(res: Response): void {
  if (res.status === 401) {
    location.reload();
    throw new Error('unauthorized');
  }
}

async function getJSON<T>(url: string): Promise<T> {
  const res = await fetch(url);
  checkAuth(res);
  if (!res.ok) throw new Error(`${url}: ${res.status}`);
  return res.json();
}

function el(tag: string, className: string, text?: string): HTMLElement {
  const e = document.createElement(tag);
  if (className) e.className = className;
  if (text !== undefined) e.textContent = text;
  return e;
}

// --- time formatting --------------------------------------------------------

function fmtFull(iso: string): string {
  const d = new Date(iso);
  return isNaN(d.getTime())
    ? iso
    : d.toLocaleString('nl-NL', { dateStyle: 'full', timeStyle: 'short' });
}

function fmtRelative(iso: string): string {
  const d = new Date(iso);
  if (isNaN(d.getTime())) return iso;
  const mins = Math.floor((Date.now() - d.getTime()) / 60_000);
  if (mins < 1) return 'zojuist';
  if (mins < 60) return `${mins} min geleden`;
  const hours = Math.floor(mins / 60);
  if (hours < 24 && d.getDate() === new Date().getDate()) return `${hours} uur geleden`;
  const yesterday = new Date();
  yesterday.setDate(yesterday.getDate() - 1);
  const time = d.toLocaleTimeString('nl-NL', { hour: '2-digit', minute: '2-digit' });
  if (d.toDateString() === yesterday.toDateString()) return `gisteren ${time}`;
  return `${d.toLocaleDateString('nl-NL', { day: 'numeric', month: 'short' })} ${time}`;
}

function fmtClock(seconds: number): string {
  if (!isFinite(seconds)) return '0:00';
  const m = Math.floor(seconds / 60);
  const s = Math.floor(seconds % 60).toString().padStart(2, '0');
  return `${m}:${s}`;
}

// --- header health + stats --------------------------------------------------

async function refreshStatus(): Promise<void> {
  const health = $('#health');
  const text = $('.health-text');
  try {
    const s = await getJSON<Status>('/api/status');
    const pollAge = (Date.now() - new Date(s.watcher.last_poll).getTime()) / 1000;
    if (s.watcher.last_error) {
      health.className = 'health err';
      text.textContent = `storing: ${s.watcher.last_error.slice(0, 60)}`;
      text.title = s.watcher.last_error;
    } else if (s.watcher.last_poll && pollAge < 150) {
      health.className = 'health ok';
      text.textContent = 'actief';
      text.title = '';
    } else {
      health.className = 'health warn';
      text.textContent = s.watcher.last_poll ? 'poll loopt achter' : 'wachten op eerste poll';
    }
    $('#stat-total .stat-value').textContent = String(s.voicemail_count);
    $('#stat-poll .stat-value').textContent = s.watcher.last_poll
      ? fmtRelative(s.watcher.last_poll)
      : '–';
    $('#logout').hidden = !s.auth_enabled;
  } catch {
    health.className = 'health err';
    text.textContent = '⚠ geen verbinding';
  }
}

function refreshStats(): void {
  const now = new Date();
  const startOfDay = new Date(now.getFullYear(), now.getMonth(), now.getDate()).getTime();
  // ISO week starts on Monday.
  const dow = (now.getDay() + 6) % 7;
  const startOfWeek = startOfDay - dow * 86_400_000;
  let today = 0;
  let week = 0;
  for (const vm of voicemails) {
    const t = new Date(vm.received_at).getTime();
    if (t >= startOfDay) today++;
    if (t >= startOfWeek) week++;
  }
  $('#stat-today .stat-value').textContent = String(today);
  $('#stat-week .stat-value').textContent = String(week);
  $('#stat-open .stat-value').textContent = String(voicemails.filter(vm => !vm.done).length);
}

// --- chart ------------------------------------------------------------------

const SVG_NS = 'http://www.w3.org/2000/svg';

function svgEl(tag: string, attrs: Record<string, string>): SVGElement {
  const e = document.createElementNS(SVG_NS, tag);
  for (const [k, v] of Object.entries(attrs)) e.setAttribute(k, v);
  return e;
}

function roundedBarPath(x: number, y: number, w: number, h: number, r: number): string {
  // Rounded top corners only, flat base anchored to the baseline.
  const rr = Math.min(r, w / 2, h);
  return `M${x},${y + h} V${y + rr} Q${x},${y} ${x + rr},${y} H${x + w - rr} Q${x + w},${y} ${x + w},${y + rr} V${y + h} Z`;
}

function renderChart(): void {
  const box = $('#chart');
  const tooltip = $('#tooltip');
  box.replaceChildren();

  const DAYS = 14;
  const now = new Date();
  const counts: number[] = new Array(DAYS).fill(0);
  const labels: Date[] = [];
  for (let i = 0; i < DAYS; i++) {
    const d = new Date(now.getFullYear(), now.getMonth(), now.getDate() - (DAYS - 1 - i));
    labels.push(d);
  }
  const start = labels[0].getTime();
  for (const vm of voicemails) {
    const t = new Date(vm.received_at).getTime();
    const idx = Math.floor((t - start) / 86_400_000);
    if (idx >= 0 && idx < DAYS) counts[idx]++;
  }

  const W = Math.max(box.clientWidth, 320);
  const H = 132;
  const padL = 22;
  const padB = 18;
  const padT = 8;
  const plotW = W - padL - 6;
  const plotH = H - padT - padB;
  const max = Math.max(4, ...counts);

  const svg = svgEl('svg', { viewBox: `0 0 ${W} ${H}`, role: 'img' });
  svg.setAttribute('aria-label', 'Aantal voicemails per dag, laatste 14 dagen');

  // Recessive grid: two lines plus baseline, labeled in the left gutter.
  for (const frac of [0.5, 1]) {
    const val = Math.round(max * frac);
    const y = padT + plotH - (val / max) * plotH;
    svg.append(svgEl('line', { x1: String(padL), x2: String(W - 6), y1: String(y), y2: String(y), class: 'grid-line' }));
    const t = svgEl('text', { x: String(padL - 5), y: String(y + 3.5), 'text-anchor': 'end' });
    t.textContent = String(val);
    svg.append(t);
  }
  svg.append(svgEl('line', {
    x1: String(padL), x2: String(W - 6),
    y1: String(padT + plotH), y2: String(padT + plotH), class: 'baseline',
  }));

  const slot = plotW / DAYS;
  const barW = Math.max(6, Math.min(26, slot - 2)); // ≥2px gap between bars
  counts.forEach((count, i) => {
    const x = padL + i * slot + (slot - barW) / 2;
    const h = count === 0 ? 0 : Math.max(3, (count / max) * plotH);
    const y = padT + plotH - h;

    const col = svgEl('g', { class: 'col' });
    if (count > 0) {
      col.append(svgEl('path', { d: roundedBarPath(x, y, barW, h, 4), class: 'bar' }));
    }
    // Full-height hit target so hover works on short/empty bars too.
    const hit = svgEl('rect', {
      x: String(padL + i * slot), y: String(padT),
      width: String(slot), height: String(plotH), class: 'hit',
    });
    const label = labels[i].toLocaleDateString('nl-NL', { weekday: 'short', day: 'numeric', month: 'short' });
    hit.addEventListener('mousemove', (ev: Event) => {
      const me = ev as MouseEvent;
      tooltip.hidden = false;
      tooltip.innerHTML = '';
      tooltip.append(el('span', '', `${label}: `));
      const b = document.createElement('b');
      b.textContent = `${count} voicemail${count === 1 ? '' : 's'}`;
      tooltip.append(b);
      tooltip.style.left = `${Math.min(me.clientX + 12, window.innerWidth - tooltip.offsetWidth - 8)}px`;
      tooltip.style.top = `${me.clientY - 34}px`;
    });
    hit.addEventListener('mouseleave', () => { tooltip.hidden = true; });
    col.append(hit);
    svg.append(col);

    // Sparse x labels, anchored at the newest day and stepping back with a
    // width-aware interval so labels never collide on narrow screens.
    const labelStep = Math.max(2, Math.ceil(40 / slot));
    if ((DAYS - 1 - i) % labelStep === 0) {
      const t = svgEl('text', {
        x: String(padL + i * slot + slot / 2),
        y: String(H - 4),
        'text-anchor': 'middle',
      });
      t.textContent = labels[i].toLocaleDateString('nl-NL', { day: 'numeric', month: 'numeric' });
      svg.append(t);
    }
  });

  box.append(svg);
}

// --- voicemail list ---------------------------------------------------------

function highlight(target: HTMLElement, text: string, query: string): void {
  if (!query) {
    target.textContent = text;
    return;
  }
  target.replaceChildren();
  const lower = text.toLowerCase();
  const q = query.toLowerCase();
  let pos = 0;
  for (let idx = lower.indexOf(q); idx !== -1; idx = lower.indexOf(q, pos)) {
    target.append(text.slice(pos, idx));
    target.append(el('mark', '', text.slice(idx, idx + q.length)));
    pos = idx + q.length;
  }
  target.append(text.slice(pos));
}

function buildPlayer(vm: Voicemail): HTMLElement {
  const wrap = el('div', 'player');
  const play = document.createElement('button');
  play.className = 'play';
  play.replaceChildren(icon(Play, 15));
  play.title = 'Afspelen';
  const seek = document.createElement('input');
  seek.type = 'range';
  seek.className = 'seek';
  seek.min = '0';
  seek.max = '100';
  seek.value = '0';
  const time = el('span', 'time', '0:00');
  const dl = document.createElement('a');
  dl.className = 'dl';
  dl.href = `/api/voicemails/${vm.id}/audio`;
  dl.download = `voicemail-${vm.id}.wav`;
  dl.title = 'Download';
  dl.append(icon(Download, 15));

  let audio: HTMLAudioElement | null = null;
  const ensureAudio = (): HTMLAudioElement => {
    if (audio) return audio;
    audio = new Audio(`/api/voicemails/${vm.id}/audio`);
    audio.preload = 'metadata';
    audio.addEventListener('loadedmetadata', () => {
      time.textContent = `0:00 / ${fmtClock(audio!.duration)}`;
    });
    audio.addEventListener('timeupdate', () => {
      if (audio!.duration > 0) seek.value = String((audio!.currentTime / audio!.duration) * 100);
      time.textContent = `${fmtClock(audio!.currentTime)} / ${fmtClock(audio!.duration)}`;
    });
    audio.addEventListener('ended', () => { play.replaceChildren(icon(Play, 15)); seek.value = '0'; });
    audio.addEventListener('pause', () => { play.replaceChildren(icon(Play, 15)); });
    audio.addEventListener('play', () => { play.replaceChildren(icon(Pause, 15)); });
    return audio;
  };

  play.addEventListener('click', () => {
    const a = ensureAudio();
    if (a.paused) {
      if (currentAudio && currentAudio !== a) currentAudio.pause();
      currentAudio = a;
      void a.play();
    } else {
      a.pause();
    }
  });
  seek.addEventListener('input', () => {
    const a = ensureAudio();
    if (a.duration > 0) a.currentTime = (Number(seek.value) / 100) * a.duration;
  });

  wrap.append(play, seek, time, dl);
  return wrap;
}

async function setDone(vm: Voicemail, done: boolean, btn: HTMLButtonElement): Promise<void> {
  btn.disabled = true;
  try {
    const res = await fetch(`/api/voicemails/${vm.id}/done`, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ done }),
    });
    checkAuth(res);
    if (!res.ok) throw new Error(String(res.status));
    const updated = (await res.json()) as Voicemail;
    const idx = voicemails.findIndex(v => v.id === updated.id);
    if (idx !== -1) voicemails[idx] = updated;
    renderVoicemails(($('#search') as HTMLInputElement).value);
    refreshStats();
    updateFilterCounts();
    void refreshEvents();
  } catch {
    toast(`Bijwerken van #${vm.id} mislukt.`, false);
    btn.disabled = false;
  }
}

function doneButton(vm: Voicemail): HTMLButtonElement {
  const btn = document.createElement('button');
  if (vm.done) {
    btn.className = 'done-btn is-done';
    btn.append(icon(Check, 13), 'Afgehandeld');
    if (vm.done_at) btn.title = `Afgehandeld ${fmtRelative(vm.done_at)} — klik om te heropenen`;
    else btn.title = 'Klik om te heropenen';
  } else {
    btn.className = 'done-btn';
    btn.append(icon(Check, 13), 'Afhandelen');
    btn.title = 'Markeer als afgehandeld';
  }
  btn.addEventListener('click', () => void setDone(vm, !vm.done, btn));
  return btn;
}

// The PBX mail body contains a "Klantnaam: <naam>" line; surface it as the
// card title.
function extractCustomer(emailText: string): string {
  const m = emailText.match(/^\s*klantnaam\s*:\s*(.+)$/im);
  return m ? m[1].trim() : '';
}

// The caller is in a line like: From:    "0657553610" <0657553610>
function extractCaller(emailText: string): string {
  const quoted = emailText.match(/^\s*from\s*:\s*"([^"\n]+)"/im);
  if (quoted) return quoted[1].trim();
  const angled = emailText.match(/^\s*from\s*:.*<([^>\n]+)>/im);
  return angled ? angled[1].trim() : '';
}

function renderVoicemails(query: string): void {
  const box = $('#voicemails');
  box.replaceChildren();
  const q = query.trim().toLowerCase();
  const shown = voicemails.filter(vm => {
    if (filter === 'open' && vm.done) return false;
    if (filter === 'done' && !vm.done) return false;
    return !q ||
      vm.transcription.toLowerCase().includes(q) ||
      vm.subject.toLowerCase().includes(q) ||
      vm.email_text.toLowerCase().includes(q);
  });
  if (shown.length === 0) {
    const msg = q ? 'Geen resultaten voor deze zoekopdracht.'
      : filter === 'open' ? 'Geen open voicemails — alles is afgehandeld.'
      : filter === 'done' ? 'Nog niets afgehandeld.'
      : 'Nog geen voicemails ontvangen.';
    box.append(el('div', 'empty card', msg));
    return;
  }
  const recent = Date.now() - 24 * 3600_000;
  for (const vm of shown) {
    const isNew = !vm.done && new Date(vm.received_at).getTime() > recent;
    const card = el('article', `vm${isNew ? ' new' : ''}${vm.done ? ' done' : ''}`);

    const meta = el('div', 'meta');
    const when = el('span', 'when', fmtRelative(vm.received_at));
    when.title = fmtFull(vm.received_at);
    meta.append(when, el('span', 'spacer'), el('span', '', `#${vm.id}`), doneButton(vm));
    card.append(meta);

    const customer = extractCustomer(vm.email_text);
    const caller = extractCaller(vm.email_text);
    if (customer || caller) {
      const row = el('div', 'title-row');
      if (customer) {
        const title = el('span', 'customer');
        highlight(title, customer, q);
        row.append(title);
      }
      if (caller) {
        const phone = document.createElement('a');
        phone.className = 'phone';
        phone.href = `tel:${caller.replace(/[^\d+]/g, '')}`;
        phone.title = 'Terugbellen';
        phone.append(icon(Phone, 13));
        const num = el('span', '');
        highlight(num, caller, q);
        phone.append(num);
        row.append(phone);
      }
      card.append(row);
    }
    if (vm.subject) {
      const subject = el('div', 'subject');
      highlight(subject, vm.subject, q);
      card.append(subject);
    }

    const transcript = el('div', `transcript${vm.transcription ? '' : ' empty'}`);
    if (vm.transcription) highlight(transcript, vm.transcription, q);
    else transcript.textContent = 'Geen transcriptie beschikbaar';
    card.append(transcript);

    if (vm.email_text) {
      const details = document.createElement('details');
      const summary = document.createElement('summary');
      summary.textContent = 'E-mail details';
      const p = document.createElement('p');
      highlight(p, vm.email_text, q);
      details.append(summary, p);
      card.append(details);
    }

    if (vm.has_audio) card.append(buildPlayer(vm));
    box.append(card);
  }
}

function updateFilterCounts(): void {
  const open = voicemails.filter(vm => !vm.done).length;
  const btn = document.querySelector<HTMLButtonElement>('#filters [data-filter="open"]');
  if (btn) btn.textContent = open > 0 ? `Open (${open})` : 'Open';
}

async function refreshVoicemails(): Promise<void> {
  try {
    voicemails = await getJSON<Voicemail[]>('/api/voicemails?limit=500');
    renderVoicemails(($('#search') as HTMLInputElement).value);
    refreshStats();
    updateFilterCounts();
    renderChart();
  } catch (e) {
    console.error('failed to load voicemails', e);
  }
}

// --- actions ----------------------------------------------------------------

let toastTimer = 0;
function toast(message: string, ok: boolean): void {
  const t = $('#toast');
  t.textContent = message;
  t.className = `toast ${ok ? 'ok' : 'err'}`;
  t.hidden = false;
  clearTimeout(toastTimer);
  toastTimer = window.setTimeout(() => { t.hidden = true; }, 5000);
}

async function runAction(name: string, button: HTMLButtonElement): Promise<void> {
  button.disabled = true;
  try {
    const res = await fetch(`/api/actions/${encodeURIComponent(name)}`, { method: 'POST' });
    checkAuth(res);
    if (!res.ok) throw new Error(String(res.status));
    const data = (await res.json()) as { ok: boolean; message: string };
    toast(data.message, data.ok);
  } catch {
    toast(`Actie "${name}" mislukt.`, false);
  } finally {
    button.disabled = false;
    void refreshEvents();
  }
}

function actionButton(action: Action): HTMLButtonElement {
  const iconNode = action.kind === 'storingsdienst' || action.group === 'storingsdienst'
    ? PhoneForwarded
    : /delete|verwijder/i.test(action.name) ? Trash2
    : Wrench;
  const label = action.kind === 'storingsdienst'
    ? `Storingsdienst → ${action.name}`
    : action.name;
  const btn = document.createElement('button');
  if (action.primary) btn.className = 'primary';
  const setLabel = (): void => btn.replaceChildren(icon(iconNode, 15), label);
  setLabel();
  let confirming = false;
  let resetTimer = 0;
  const reset = (): void => {
    confirming = false;
    btn.classList.remove('confirming');
    setLabel();
  };
  // Two-step inline confirmation instead of a blocking confirm() dialog.
  btn.addEventListener('click', () => {
    if (!confirming) {
      confirming = true;
      btn.classList.add('confirming');
      btn.replaceChildren(icon(TriangleAlert, 15), 'Zeker weten? Klik nogmaals');
      clearTimeout(resetTimer);
      resetTimer = window.setTimeout(reset, 4000);
      return;
    }
    clearTimeout(resetTimer);
    reset();
    void runAction(action.name, btn);
  });
  return btn;
}

async function loadActions(): Promise<void> {
  const box = $('#actions');
  try {
    const list = await getJSON<Action[]>('/api/actions');
    if (list.length === 0) {
      $('#actions-section').hidden = true;
      return;
    }
    // Ungrouped actions on top (primary first, per API order); grouped ones
    // in collapsible sections that are collapsed by default and remember
    // their state. The storingsdienst group holds the phone-number switches
    // plus any command with `group: storingsdienst` (vivia, avics).
    const groups = new Map<string, Action[]>();
    for (const action of list) {
      if (!action.group) {
        box.append(actionButton(action));
        continue;
      }
      const members = groups.get(action.group) ?? [];
      members.push(action);
      groups.set(action.group, members);
    }
    for (const [name, members] of groups) {
      const group = document.createElement('details');
      group.className = 'actions-group';
      const storeKey = `actions-group-${name}`;
      group.open = localStorage.getItem(storeKey) === '1';
      group.addEventListener('toggle', () =>
        localStorage.setItem(storeKey, group.open ? '1' : '0'),
      );
      const label = name === 'storingsdienst'
        ? 'Storingsdienst omzetten'
        : name.charAt(0).toUpperCase() + name.slice(1);
      const summary = document.createElement('summary');
      summary.append(icon(ChevronRight, 14), `${label} (${members.length})`);
      const body = el('div', 'group-body');
      // Commands (vivia, avics) above the individual phone-number switches.
      members.sort((a, b) =>
        a.kind === b.kind ? a.name.localeCompare(b.name) : a.kind === 'command' ? -1 : 1,
      );
      for (const action of members) body.append(actionButton(action));
      group.append(summary, body);
      box.append(group);
    }
  } catch {
    $('#actions-section').hidden = true;
  }
}

// --- events -----------------------------------------------------------------

const EVENT_ICONS: Record<string, IconNode> = {
  voicemail: VoicemailIcon,
  command: Wrench,
  cleanup: Archive,
  startup: Rocket,
  error: TriangleAlert,
  done: CircleCheck,
};

async function refreshEvents(): Promise<void> {
  const box = $('#events');
  try {
    const events = await getJSON<AppEvent[]>('/api/events?limit=30');
    box.replaceChildren();
    if (events.length === 0) {
      box.append(el('div', 'empty', 'Nog geen activiteit.'));
      return;
    }
    for (const ev of events) {
      const row = el('div', `event${ev.kind === 'error' ? ' error' : ''}`);
      const iconBox = el('span', 'event-icon');
      iconBox.append(icon(EVENT_ICONS[ev.kind] ?? Inbox, 14));
      row.append(iconBox);
      const body = el('div', 'body');
      body.append(el('div', 'detail', ev.detail || ev.kind));
      const at = el('div', 'at', fmtRelative(ev.at));
      at.title = fmtFull(ev.at);
      body.append(at);
      row.append(body);
      box.append(row);
    }
  } catch (e) {
    console.error('failed to load events', e);
  }
}

// --- init -------------------------------------------------------------------

$('.brand-icon').append(icon(VoicemailIcon, 22));
$('#logout').append(icon(LogOut, 15));

$('#search').addEventListener('input', e =>
  renderVoicemails((e.target as HTMLInputElement).value),
);

for (const btn of document.querySelectorAll<HTMLButtonElement>('#filters button')) {
  btn.addEventListener('click', () => {
    filter = (btn.dataset.filter ?? 'all') as Filter;
    for (const b of document.querySelectorAll('#filters button')) b.classList.toggle('active', b === btn);
    renderVoicemails(($('#search') as HTMLInputElement).value);
  });
}

let resizeTimer = 0;
window.addEventListener('resize', () => {
  clearTimeout(resizeTimer);
  resizeTimer = window.setTimeout(renderChart, 150);
});

void loadActions();
void refreshStatus();
void refreshVoicemails();
void refreshEvents();

setInterval(() => {
  void refreshStatus();
  void refreshVoicemails();
  void refreshEvents();
}, 30_000);
