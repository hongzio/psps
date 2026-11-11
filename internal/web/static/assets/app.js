// psps web UI. Plain DOM, no framework, no build step.
//
// Everything an agent says is untrusted (a repository or a web page can put
// words in its mouth), so agent text only ever reaches the page through
// text nodes: h() below never parses HTML, and markdown is only lexed by
// marked (marked.esm.js, marked 18.1.1 from npm as published) and drawn
// here, never made into HTML.

import { Lexer } from './marked.esm.js'
import { icons } from './icons.js'

const root = document.getElementById('app')

function h(tag, props, ...children) {
  const el = document.createElement(tag)
  for (const [k, v] of Object.entries(props || {})) {
    if (v == null || v === false) continue
    if (k === 'class') el.className = v
    else if (k.startsWith('on')) el.addEventListener(k.slice(2), v)
    else if (k === 'value' || k === 'checked' || k === 'disabled' || k === 'open') el[k] = v
    else el.setAttribute(k, v === true ? '' : v)
  }
  for (const c of children.flat(Infinity)) {
    if (c == null || c === false) continue
    el.append(c instanceof Node ? c : document.createTextNode(String(c)))
  }
  return el
}

// icon is one of icons.js, built element by element like h(): sized to
// the text around it and drawn in its color.
const svgNS = 'http://www.w3.org/2000/svg'
function icon(name) {
  const svg = document.createElementNS(svgNS, 'svg')
  for (const [k, v] of Object.entries({ class: 'icon', viewBox: '0 0 24 24', fill: 'none', stroke: 'currentColor',
    'stroke-width': '2', 'stroke-linecap': 'round', 'stroke-linejoin': 'round', 'aria-hidden': 'true' })) svg.setAttribute(k, v)
  for (const [tag, attrs] of icons[name]) {
    const el = document.createElementNS(svgNS, tag)
    for (const [k, v] of Object.entries(attrs)) el.setAttribute(k, v)
    svg.append(el)
  }
  return svg
}

// ---- API

class Unauthorized extends Error {}

async function api(method, path, body) {
  const res = await fetch(path, {
    method,
    credentials: 'same-origin',
    headers: { 'Content-Type': 'application/json', 'X-Psps-Request': '1' },
    body: body === undefined ? undefined : JSON.stringify(body),
  })
  if (res.status === 401) throw new Unauthorized()
  const data = await res.json().catch(() => ({}))
  if (!res.ok) throw new Error(data.error || res.statusText)
  return data
}

// ---- State

const S = {
  authed: true,
  agents: [],
  isolations: [],
  sel: null, // agent id
  view: 'agent', // agent | new | isolations
  showArchived: false,
  // Lines whose archived sessions are unfolded, by line.
  openLines: new Set(),
  // Teams folded to their heads in the sidebar, by ID: this browser's,
  // kept across visits.
  foldedTeams: new Set((() => { try { return JSON.parse(localStorage.getItem('psps.foldedTeams') || '[]') } catch { return [] } })()),
  // Agents whose turn ended, working to idle, while another was open, and
  // not opened since, by ID: this browser's, kept across visits. lastStatus
  // is each one's status as last seen, to tell a turn's end by.
  unread: new Set((() => { try { return JSON.parse(localStorage.getItem('psps.unread') || '[]') } catch { return [] } })()),
  lastStatus: new Map(),
  // The picker for a line's icon: the agent it is open for, the color it
  // offers, and the icons picked lately, with their colors, in this browser.
  iconFor: null,
  iconColor: '',
  iconFresh: null,
  recentIcons: (() => { try { const v = JSON.parse(localStorage.getItem('psps.recentIcons') || '[]'); return Array.isArray(v) ? v : [] } catch { return [] } })(),
  // A sidebar row is held or being moved: the sidebar is not redrawn under
  // it, but once it is let go.
  sorting: false,
  sidebarStale: false,
  // Agents picked in the sidebar, to archive, restore or delete at once:
  // null, or the group they are of (live or archived: never both), the
  // IDs picked, the one last pressed and whether that press picked it or
  // let it go, which a Shift-click carries on. picking is a press on a
  // status icon going on, which the sidebar is not redrawn under.
  pick: null,
  picking: false,
  drawer: false,
  convos: new Map(),
  // What is typed to each agent and not yet sent, kept in this browser so
  // that another agent, another page or a reload does not lose it.
  drafts: new Map((() => { try { const v = JSON.parse(localStorage.getItem('psps.drafts') || '{}'); return v && typeof v === 'object' ? Object.entries(v).filter(([, t]) => typeof t === 'string' && t) : [] } catch { return [] } })()),
  // What is picked and typed for a question an agent asks, by its ID.
  answering: {},
  es: null,
  esFor: null,
  settingsOpen: false,
  toast: null,
  // The credential proxies: services and their envs, and what each
  // isolation exposes. openForm names the one inline form that is open;
  // while it is, polling does not redraw the page under the operator.
  services: [],
  envs: [],
  exposures: {},
  // The settings pages are a list and one of it in detail, the address's:
  // an isolation by name (NEW for the form that makes one) and its tab, a
  // service by name, and on Accounts an isolation's logins or an account
  // ({ kind: 'iso' | 'account' | 'new', name }). None chosen, a wide page
  // shows the first and a narrow one the list.
  isoSel: null,
  isoTab: 'overview',
  svcSel: null,
  setupSel: null,
  openForm: null,
  login: null,
  checkOut: null,
  verifyOut: null, // the open verify report
  verifying: null, // the isolation being measured
  starting: null, // the isolation being started
  tasksOpen: null, // the agent whose background tasks are listed
  // Claude & Codex: each isolation's CLIs as read from inside it (an
  // array, or {error}), the login on screen, the CLI an action runs for.
  clis: {},
  cliLogin: null, // { iso, tool, login }
  cliBusy: null, // `${iso}/${tool}`
  accounts: null, // agent CLI accounts, shared by isolations
  accountKinds: [],
  // The New isolation form: what is typed, the one being created, and why
  // the last one was not.
  isoDraft: { name: '', memory: '4G', fence: true },
  acctDraft: { name: '', kind: null, secret: '' },
  isoCreating: null,
  isoCreateError: null,
  // edit is the user message being rewritten in place: { agent, id, text,
  // sending }. openRewound holds the earlier versions shown unfolded,
  // openTools the runs of one tool likewise, openLong the long messages
  // the person sent that they opened.
  edit: null,
  openRewound: new Set(),
  openTools: new Set(),
  openLong: new Set(),
}

const coarse = matchMedia('(pointer: coarse)').matches
const agentById = (id) => S.agents.find((a) => a.id === id)
// agentTitle is what an agent is called on the page: one made without a
// prompt has no name until its first message.
const agentTitle = (a) => a.name || 'New agent'
// shownID is the ID an agent goes by: its own, the one it went by until a
// new session took it, or the key psps gave it.
const shownID = (a) => a.handle || a.was || a.id
// letter reads a mail from another agent as the agent got it (the daemon's
// mailproxy.Letter): who wrote it, from where, and what it says.
const LETTER = /^\[psps mail #(\d+) from agent (\S+) in isolation (\S+)\. [^\n]*\]\n\n/
function letter(text) {
  const m = LETTER.exec(text)
  return m && { id: m[1], from: m[2], iso: m[3], body: text.slice(m[0].length) }
}
// A cron's message as its agent got it (internal/daemon/crons.go).
const CRON = /^\[psps cron #(\d+), on schedule "([^"\n]*)"\. [^\n]*\]\n\n/
function cronMsg(text) {
  const m = CRON.exec(text)
  // One the agent set itself says so, and one another set through the
  // control service says whose it is (cronLetter).
  const by = m && /Agent (.+?) set this message, through psps's control service/.exec(m[0])
  return m && { id: m[1], schedule: m[2], self: m[0].includes('You set this message yourself'), by: by?.[1], body: text.slice(m[0].length) }
}

// Receipts psps sends an agent of what came of what it sent through the
// control service (internal/daemon/watches.go).
const RECEIPTS = /^\[psps receipts: [^\n]*\]\n\n/
function receiptsMsg(text) {
  const m = RECEIPTS.exec(text)
  return m && { body: text.slice(m[0].length) }
}

// A schedule as the cron form has it: every N minutes or hours, a time on
// some days, or a cron expression as written. Days are cron's: 0 is Sunday.
const DAYS = [[1, 'Mon'], [2, 'Tue'], [3, 'Wed'], [4, 'Thu'], [5, 'Fri'], [6, 'Sat'], [0, 'Sun']]
const EVERY = /^@every (\d+)([mh])$/
const AT = /^(\d{1,2}) (\d{1,2}) \* \* (\*|[0-7](?:,[0-7])*)$/
function scheduleParts(spec) {
  let m = EVERY.exec(spec)
  if (m && +m[1] > 0) return { kind: 'every', n: +m[1], unit: m[2] }
  m = AT.exec(spec)
  if (m && +m[1] < 60 && +m[2] < 24) {
    const days = m[3] === '*' ? DAYS.map(([d]) => d) : [...new Set(m[3].split(',').map((d) => +d % 7))]
    return { kind: 'at', hour: +m[2], minute: +m[1], days }
  }
  return { kind: 'cron', expr: spec }
}
function scheduleSpec(p) {
  if (p.kind === 'every') return `@every ${p.n}${p.unit}`
  if (p.kind === 'at') return `${p.minute} ${p.hour} * * ${p.days.length === 7 ? '*' : [...p.days].sort().join(',')}`
  return p.expr.trim()
}
const pad2 = (n) => String(n).padStart(2, '0')
const NAMED_SCHEDULES = {
  '@hourly': 'Every hour, on the hour', '@daily': 'Every day at 00:00', '@midnight': 'Every day at 00:00',
  '@weekly': 'Sundays at 00:00', '@monthly': 'The 1st of every month at 00:00', '@yearly': 'January 1 at 00:00', '@annually': 'January 1 at 00:00',
}
function describeSchedule(spec) {
  const p = scheduleParts(spec)
  if (p.kind === 'every') {
    const unit = p.unit === 'm' ? 'minute' : 'hour'
    return p.n === 1 ? `Every ${unit}` : `Every ${p.n} ${unit}s`
  }
  if (p.kind === 'at') {
    const only = (ds) => ds.length === p.days.length && ds.every((d) => p.days.includes(d))
    const on = p.days.length === 7 ? 'Every day' : only([1, 2, 3, 4, 5]) ? 'Weekdays' : only([0, 6]) ? 'Weekends'
      : DAYS.filter(([d]) => p.days.includes(d)).map(([, n]) => n).join(', ')
    return `${on} at ${pad2(p.hour)}:${pad2(p.minute)}`
  }
  return NAMED_SCHEDULES[spec.toLowerCase()] || `cron ${spec}`
}

const label = (a) => (a.kind === 'codex' ? 'Codex' : 'Claude')
const effortOptions = (kind) =>
  (kind === 'codex' ? ['', 'low', 'medium', 'high', 'xhigh'] : ['', 'low', 'medium', 'high', 'xhigh', 'max'])
    .map((e) => ({ value: e, label: e || 'default' }))

function toast(msg, err) {
  S.toast = { msg, err }
  renderToast()
  clearTimeout(toast.t)
  toast.t = setTimeout(() => { S.toast = null; renderToast() }, 4000)
}

async function guarded(fn) {
  try {
    return await fn()
  } catch (e) {
    if (e instanceof Unauthorized) { S.authed = false; render(); return }
    console.error(e)
    toast(e.message, true)
  }
}

// ask is the page's own confirm(): a modal with the question, what follows
// from it, and a button named for the action. It resolves true when the
// operator takes the action; Esc, Cancel or a click outside leave it be.
// A destructive one starts on Cancel, so a stray Enter does nothing. extra
// is shown under the body: a choice the caller reads once it resolves.
// wide is for one that holds a form of its own. fields hold text inputs
// for the answer instead: the first starts
// focused, and Enter in any takes the action. onOk, if given, does the
// action: what it throws is shown in the dialog, which stays open. Without
// ok it is only something to look at, closed with cancel ("Close").
function ask({ title, body, ok, cancel: cancelLabel = 'Cancel', danger, extra, fields, onOk, wide }) {
  return new Promise((resolve) => {
    const cancel = h('button', { class: 'btn', value: 'cancel' }, cancelLabel)
    const go = ok ? h('button', { class: 'btn primary' + (danger ? ' danger' : ''), value: 'ok' }, ok) : null
    const err = h('p', { class: 'form-error dialog-error', role: 'alert', hidden: true })
    const d = h('dialog', { class: 'dialog' + (wide ? ' wide' : ''), 'aria-labelledby': 'dialog-title', 'aria-describedby': body ? 'dialog-body' : null },
      h('form', { method: 'dialog' },
        h('h3', { id: 'dialog-title' }, title),
        body ? h('p', { id: 'dialog-body' }, body) : null,
        extra || null,
        ...(fields || []),
        err,
        h('div', { class: 'dialog-actions' }, cancel, go)))
    const take = async () => {
      if (!onOk) { d.close('ok'); return }
      go.disabled = true
      try {
        await onOk()
        d.close('ok')
      } catch (e) {
        err.textContent = e.message
        err.hidden = false
        go.disabled = false
      }
    }
    if (onOk && go) go.addEventListener('click', (e) => { e.preventDefault(); take() })
    // Outside is the backdrop, which is the dialog itself as a target; a
    // drag that starts inside and ends outside is not a click outside.
    let downOn = null
    d.addEventListener('pointerdown', (e) => { downOn = e.target })
    d.addEventListener('click', (e) => { if (e.target === d && downOn === d) d.close('cancel') })
    d.addEventListener('close', () => { d.remove(); resolve(d.returnValue === 'ok') })
    const inputs = (fields || []).flatMap((f) => f.matches('input') ? [f] : [...f.querySelectorAll('input')])
    for (const i of inputs) {
      i.addEventListener('keydown', (e) => {
        if (e.key === 'Enter' && !e.isComposing) { e.preventDefault(); if (go && !go.disabled) take() }
      })
    }
    document.body.append(d)
    d.showModal()
    if (inputs.length) inputs[0].select()
    else (danger || !go ? cancel : go).focus()
  })
}

// dropdown is the page's own <select>: a button showing the choice that
// opens a list of the options. It keeps a select's surface, so forms read
// it the same way: value (get and set), disabled, a bubbling change event
// when the operator picks something else, and setOptions to refill it,
// keeping the choice if it is still there. Options are strings or
// {value, label}. The list is drawn on the body (in a dialog, on the
// dialog), so no scrolling page or popover clips it, follows the button
// while open, and shuts when the button leaves the page. Keys follow a select-only combobox: arrows,
// Home/End, typing a label's first letters, Enter or Space to pick, Esc.
function dropdown(options, { value, label, disabled } = {}) {
  const id = 'dropdown-' + ++dropdown.n
  const shown = h('span', { class: 'dropdown-value' })
  const btn = h('button', {
    type: 'button', class: 'dropdown', role: 'combobox', 'aria-haspopup': 'listbox', 'aria-expanded': 'false',
    'aria-controls': id, 'aria-label': label, disabled,
  }, shown)
  let opts = []
  let cur = ''
  let list = null
  let active = -1
  let frame = 0
  let typed = ''
  let typedAt = 0

  const at = (v) => opts.findIndex((o) => o.value === v)
  const set = (v) => {
    cur = at(v) >= 0 ? v : (opts[0]?.value ?? '')
    const o = opts[at(cur)]
    shown.textContent = o ? o.label : ''
  }
  const fill = () => list.replaceChildren(...opts.map((o, i) =>
    h('div', { role: 'option', id: `${id}-${i}`, class: 'dropdown-option', 'aria-selected': String(o.value === cur) }, o.label)))
  const highlight = (i) => {
    if (!opts.length) return
    active = Math.max(0, Math.min(opts.length - 1, i))
    for (const [j, el] of [...list.children].entries()) el.classList.toggle('active', j === active)
    const el = list.children[active]
    btn.setAttribute('aria-activedescendant', el.id)
    if (el.offsetTop < list.scrollTop) list.scrollTop = el.offsetTop
    else if (el.offsetTop + el.offsetHeight > list.scrollTop + list.clientHeight) list.scrollTop = el.offsetTop + el.offsetHeight - list.clientHeight
  }
  // place puts the list under the button, or over it when there is more
  // room there, inside the viewport.
  const place = () => {
    if (!btn.isConnected || btn.disabled || !btn.getClientRects().length) return close()
    const r = btn.getBoundingClientRect()
    const vw = document.documentElement.clientWidth
    const gap = 4
    const edge = 8
    const below = innerHeight - r.bottom - gap - edge
    const above = r.top - gap - edge
    list.style.minWidth = r.width + 'px'
    // The side goes by the list's whole height, borders and all, before it
    // is clipped: a clipped list grows a scrollbar, which can wrap a long
    // option, and measured that way it would change sides every frame.
    list.style.maxHeight = ''
    const up = Math.min(list.getBoundingClientRect().height, 320) > below && above > below
    list.style.maxHeight = Math.max(80, Math.min(320, up ? above : below)) + 'px'
    list.style.left = Math.max(edge, Math.min(r.left, vw - list.offsetWidth - edge)) + 'px'
    list.style.top = up ? '' : r.bottom + gap + 'px'
    list.style.bottom = up ? innerHeight - r.top + gap + 'px' : ''
  }
  const track = () => { place(); if (list) frame = requestAnimationFrame(track) }
  const outside = (e) => { if (!btn.contains(e.target) && !list?.contains(e.target)) close() }
  const open = () => {
    if (list || btn.disabled || !opts.length) return
    list = h('div', { class: 'dropdown-list', role: 'listbox', id, 'aria-label': label })
    fill()
    // The focus stays on the button, which keeps the keys and the
    // highlighted option; a click on an option picks it.
    list.addEventListener('mousedown', (e) => e.preventDefault())
    list.addEventListener('click', (e) => {
      const o = e.target.closest('[role=option]')
      if (o) choose([...list.children].indexOf(o))
    })
    list.addEventListener('pointermove', (e) => {
      const o = e.target.closest('[role=option]')
      if (o) highlight([...list.children].indexOf(o))
    })
    // A modal dialog is over the page: in one, the list is the dialog's.
    ;(btn.closest('dialog') || document.body).append(list)
    btn.setAttribute('aria-expanded', 'true')
    btn.focus({ preventScroll: true })
    document.addEventListener('pointerdown', outside, true)
    track()
    highlight(Math.max(0, at(cur)))
  }
  const close = () => {
    if (!list) return
    cancelAnimationFrame(frame)
    list.remove()
    list = null
    active = -1
    typed = ''
    btn.setAttribute('aria-expanded', 'false')
    btn.removeAttribute('aria-activedescendant')
    document.removeEventListener('pointerdown', outside, true)
  }
  const choose = (i) => {
    const o = opts[i]
    close()
    if (o && o.value !== cur) {
      set(o.value)
      btn.dispatchEvent(new Event('change', { bubbles: true }))
    }
  }
  // seek moves to the next option whose label starts with what was typed
  // in the last moment, since the list opened or the arrows last moved; one
  // letter again goes on to the next such option.
  const seek = (ch) => {
    const now = Date.now()
    typed = now - typedAt < 700 ? typed + ch.toLowerCase() : ch.toLowerCase()
    typedAt = now
    const n = opts.length
    const from = typed.length === 1 ? active + 1 : active
    for (let j = 0; j < n; j++) {
      const i = (((from + j) % n) + n) % n
      if (opts[i].label.toLowerCase().startsWith(typed)) return highlight(i)
    }
  }

  btn.addEventListener('click', () => (list ? close() : open()))
  btn.addEventListener('blur', close)
  btn.addEventListener('keydown', (e) => {
    const k = e.key
    if (e.ctrlKey || e.metaKey) return
    if (!list) {
      if (k === 'ArrowDown' || k === 'ArrowUp' || k === 'Enter' || k === ' ') { e.preventDefault(); open() }
      else if (k.length === 1 && !e.altKey) { e.preventDefault(); open(); seek(k) }
      return
    }
    if (k.length > 1) typed = ''
    switch (k) {
      case 'ArrowDown': highlight(active + 1); break
      case 'ArrowUp': if (e.altKey) choose(active); else highlight(active - 1); break
      case 'Home': case 'PageUp': highlight(0); break
      case 'End': case 'PageDown': highlight(opts.length - 1); break
      case 'Enter': choose(active); break
      case ' ': if (typed && Date.now() - typedAt < 700) seek(k); else choose(active); break
      case 'Escape': close(); break
      case 'Tab': close(); return
      default: if (k.length === 1) seek(k); else return
    }
    // Neither the page's own Esc nor the button's click on Space/Enter.
    e.preventDefault()
    e.stopPropagation()
  })
  btn.addEventListener('keyup', (e) => { if (e.key === ' ') e.preventDefault() })

  Object.defineProperty(btn, 'value', { get: () => cur, set })
  btn.setOptions = (next) => {
    opts = next.map((o) => (typeof o === 'string' ? { value: o, label: o } : o))
    set(cur)
    if (list) { fill(); highlight(Math.max(0, at(cur))) }
  }
  btn.setOptions(options)
  if (value !== undefined) set(value)
  return btn
}
dropdown.n = 0

// ---- Conversation: events folded into what a person reads (mirrors the TUI)

// A conversation is read from its end: first is the earliest event it has,
// more whether the daemon has any before it, and log every stored event it
// has, to fold again with the earlier when they come.
const newConvo = () => ({ lastID: 0, entries: [], byItem: new Map(), log: [], first: 0, firstSrc: 0, more: false })

function convo(id) {
  if (!S.convos.has(id)) S.convos.set(id, newConvo())
  return S.convos.get(id)
}

function apply(c, e) {
  if (e.id) {
    if (e.id <= c.lastID) return
    c.lastID = e.id
    if (!c.first) { c.first = e.id; c.firstSrc = e.src }
    c.log.push(e)
  }
  // src is the shim log record an entry came from: a rewind names what it
  // took back by those.
  // time is when the message began: an answer's first delta, not its end.
  const add = (en) => { en.src = e.src; en.time = e.time; c.entries.push(en); if (en.item) c.byItem.set(en.item, en); return en }
  switch (e.kind) {
    case 'user': add({ kind: 'user', id: e.id, text: e.text || '' }); break
    case 'delta': {
      let en = c.byItem.get(e.item)
      if (!en || en.kind !== 'assistant' || !en.live) en = add({ kind: 'assistant', item: e.item, live: true, text: '' })
      en.text += e.text || ''
      break
    }
    case 'assistant': {
      const en = c.byItem.get(e.item)
      if (en && en.kind === 'assistant' && en.live) { en.text = e.text; en.live = false }
      else add({ kind: 'assistant', item: e.item, text: e.text || '' })
      break
    }
    case 'reasoning': add({ kind: 'reasoning', text: e.text || '' }); break
    case 'tool_start':
      add({ kind: 'tool', item: e.item, name: e.tool?.name || '', summary: e.tool?.summary || '', output: '', state: 'running' })
      break
    case 'tool_output': {
      const en = c.byItem.get(e.item)
      if (en && en.kind === 'tool' && en.output.length < 16384) en.output += e.text || ''
      break
    }
    case 'tool_end': {
      let en = c.byItem.get(e.item)
      if (!en || en.kind !== 'tool') en = add({ kind: 'tool', item: e.item, name: '', summary: '', output: '', state: 'running' })
      if (e.tool) {
        if (e.tool.name) en.name = e.tool.name
        if (e.tool.summary) en.summary = e.tool.summary
        en.output = (e.tool.output || '').slice(0, 16384)
        en.state = e.tool.is_error ? 'failed' : 'ok'
      }
      break
    }
    // The approval's own kind (command, tool, ...) must not overwrite the
    // entry's: it is kept as akind.
    case 'approval': add({ ...e.approval, akind: e.approval.kind, kind: 'approval' }); break
    case 'approval_done':
      for (const en of c.entries) {
        if (en.kind === 'approval' && en.id === e.approval.id) Object.assign(en, { decision: e.approval.decision, answers: e.approval.answers })
      }
      break
    case 'status':
      // Drivers report blocked after every approval and leave it only when
      // none is open: any other status means nothing is pending. Not so
      // for psps's own (ssm), which the daemon holds, not the agent's CLI.
      if (e.status !== 'blocked') for (const en of c.entries) if (en.kind === 'approval' && !en.decision && en.akind !== 'ssm') en.decision = 'void'
      break
    case 'turn_end':
      if (e.text === 'interrupted') add({ kind: 'interrupted' })
      for (const en of c.entries) en.live = false
      break
    case 'error': add({ kind: 'error', text: e.text }); break
    case 'notice':
      if (!(e.text || '').startsWith('thread/')) add({ kind: 'notice', text: e.text })
      break
    case 'rewind': {
      // What the agent went back over stays where it was, folded.
      const { from, to } = e.rewind || {}
      const gone = (en) => en.src >= from && en.src < to
      const at = c.entries.findIndex(gone)
      if (at < 0) break
      const entries = c.entries.filter(gone)
      c.entries = c.entries.filter((en) => !gone(en))
      c.entries.splice(at, 0, { kind: 'rewound', src: from, entries })
      break
    }
  }
}

// rewindable: the agent has nothing in flight, so it can go back to an
// earlier message of its own (the daemon has the last word).
const rewindable = (a) => !a.archived_at && ['idle', 'interrupted', 'error'].includes(a.state?.status || 'idle') && !a.state?.queued

function startEdit(a, en) {
  S.edit = { agent: a.id, id: en.id, text: en.text }
  renderConvo()
  const ta = document.querySelector('#convo-inner .edit-box')
  if (ta) { ta.focus(); ta.setSelectionRange(ta.value.length, ta.value.length) }
}

function cancelEdit() {
  S.edit = null
  renderConvo()
}

async function resend(a, en) {
  const ed = S.edit
  const text = ed?.text.trim()
  if (!text || ed.sending) return
  ed.sending = true
  renderConvo()
  const ok = await guarded(async () => { await api('POST', `/v1/agents/${encodeURIComponent(a.id)}/rewind`, { event: en.id, text }); return true })
  if (S.edit === ed) S.edit = ok ? null : { ...ed, sending: false }
  renderConvo(ok)
}

const decisionText = (d) => ({ accept: 'accepted', decline: 'declined', cancel: 'declined, turn stopped', void: 'the process ended' })[d] || d
const askText = (kind) => ({ command: 'run a command', file_change: 'change files', tool: 'use a tool', question: 'know', ssm: 'run a command on an instance' })[kind] || kind

// markdown draws what an agent writes, which is GitHub markdown, from
// marked's tokens. Raw HTML in it is shown as written. Only a web address
// is a link: a path in the workspace has nowhere to go in a browser. An
// image is a link to it, never loaded: a page that fetched an agent's URL
// would carry what the agent put in it past the isolation's fence.
const mdTokens = new Map()

function markdown(text, live) {
  let toks = mdTokens.get(text)
  if (!toks) {
    try {
      toks = Lexer.lex(text, { gfm: true, breaks: true })
    } catch {
      return richText(text)
    }
    // The convo is redrawn at every delta of a message being written, too
    // often to lex every message each time; a finished one is kept.
    if (!live) {
      if (mdTokens.size >= 1000) mdTokens.clear()
      mdTokens.set(text, toks)
    }
  }
  return toks.map(mdBlock)
}

function mdBlock(t) {
  switch (t.type) {
    case 'paragraph': return h('p', null, mdInline(t.tokens))
    case 'text': return t.tokens ? mdInline(t.tokens) : entities(t.text) // a tight list item's
    case 'heading': return h(`h${t.depth}`, null, mdInline(t.tokens))
    case 'code': return codeBlock(t.text)
    case 'blockquote': return h('blockquote', null, t.tokens.map(mdBlock))
    case 'list':
      return h(t.ordered ? 'ol' : 'ul', { start: t.ordered && t.start !== 1 ? t.start : null }, t.items.map(mdItem))
    case 'table': {
      const cell = (tag) => (c, i) => h(tag, { class: t.align[i] ? `al-${t.align[i]}` : null }, mdInline(c.tokens))
      return h('div', { class: 'table-wrap' }, h('table', null,
        h('thead', null, h('tr', null, t.header.map(cell('th')))),
        t.rows.length ? h('tbody', null, t.rows.map((r) => h('tr', null, r.map(cell('td'))))) : null))
    }
    case 'hr': return h('hr')
    case 'html': return h('p', { class: 'raw' }, t.text)
    case 'space': case 'def': return null
    default: return t.raw ? h('p', { class: 'raw' }, t.raw) : null
  }
}

function mdItem(it) {
  const kids = it.tokens.filter((t) => t.type !== 'checkbox').map(mdBlock)
  if (it.task) {
    const box = h('input', { type: 'checkbox', checked: it.checked, disabled: true })
    if (kids[0] instanceof HTMLParagraphElement) kids[0].prepend(box)
    else kids.unshift(box)
  }
  return h('li', { class: it.task ? 'task' : null }, kids)
}

function mdInline(toks) {
  return (toks || []).map((t) => {
    switch (t.type) {
      case 'text': return t.tokens ? mdInline(t.tokens) : entities(t.text)
      case 'escape': return t.text
      case 'strong': return h('strong', null, mdInline(t.tokens))
      case 'em': return h('em', null, mdInline(t.tokens))
      case 'del': return h('del', null, mdInline(t.tokens))
      case 'codespan': return h('code', null, t.text)
      case 'br': return h('br')
      case 'link': return mdLink(t.href, mdInline(t.tokens), t.title)
      case 'image': return mdLink(t.href, [icon('image'), ' ' + (t.text || 'image')], t.title)
      // A table cell's line break is the one tag agents write often.
      case 'html': return /^<br\s*\/?>$/i.test(t.text) ? h('br') : t.text
      default: return t.raw || ''
    }
  })
}

function mdLink(href, kids, title) {
  if (!/^(https?|mailto):/i.test(href)) return h('span', { class: 'link-off', title: href }, kids)
  return h('a', { href, title: title || null, target: '_blank', rel: 'noopener noreferrer' }, kids)
}

// entities turns a character reference in text (&amp;, &#39;) into its
// character, as markdown has it; in code it is left as written.
const NAMED = { amp: '&', lt: '<', gt: '>', quot: '"', apos: "'", nbsp: ' ' }
const entities = (s) => s.replace(/&(?:#(\d{1,7})|#[xX]([0-9a-fA-F]{1,6})|(\w+));/g, (m, d, x, n) => {
  if (n) return NAMED[n] ?? m
  const c = d ? Number(d) : parseInt(x, 16)
  return c > 0 && c <= 0x10ffff && (c < 0xd800 || c > 0xdfff) ? String.fromCodePoint(c) : '�'
})

// codeBlock is a fenced block of a message with a button that copies it.
// The button sits outside the <pre>, so a wide block scrolls under it; with
// a pointer it shows on hover or focus, on a touch screen it is always
// there, on a row of its own above the code (app.css).
function codeBlock(text) {
  const btn = h('button', { type: 'button', class: 'copy-btn', 'aria-label': 'Copy code' }, icon('copy'), h('span', null, 'Copy'))
  const pre = h('pre', { class: 'code' }, h('code', null, text))
  let timer
  const show = (state, name, word) => {
    clearTimeout(timer)
    btn.className = 'copy-btn' + (state ? ' ' + state : '')
    btn.replaceChildren(icon(name), h('span', null, word))
    if (state) timer = setTimeout(() => show('', 'copy', 'Copy'), 2000)
  }
  btn.onclick = async (e) => {
    e.stopPropagation() // a folded message of yours would open
    try {
      await navigator.clipboard.writeText(text)
      show('done', 'check', 'Copied')
    } catch {
      // No clipboard (an http page that is not localhost): select the code so ⌘C takes it.
      const r = document.createRange()
      r.selectNodeContents(pre.firstChild)
      getSelection().removeAllRanges()
      getSelection().addRange(r)
      show('fail', 'copy', 'Press ⌘C')
    }
  }
  return h('div', { class: 'codeblock' }, pre, btn)
}

// richText renders what a person writes lightly: fenced code, `inline
// code`, **bold**, paragraphs. A line of theirs that starts with "1." or
// "#" stays as typed. Text nodes only.
function richText(text) {
  const frag = document.createDocumentFragment()
  text.split('```').forEach((part, i) => {
    if (i % 2 === 1) {
      const nl = part.indexOf('\n')
      const body = nl >= 0 && /^[\w+.-]*$/.test(part.slice(0, nl)) ? part.slice(nl + 1) : part
      frag.append(codeBlock(body.replace(/\n$/, '')))
      return
    }
    for (const para of part.split(/\n{2,}/)) {
      if (!para.trim()) continue
      const p = h('p')
      para.split('\n').forEach((line, j) => { if (j) p.append(h('br')); p.append(...inline(line)) })
      frag.append(p)
    }
  })
  return frag
}

function inline(line) {
  const out = []
  const re = /(`[^`]+`|\*\*[^*]+\*\*)/g
  let last = 0
  let m
  while ((m = re.exec(line))) {
    if (m.index > last) out.push(line.slice(last, m.index))
    const t = m[0]
    out.push(t[0] === '`' ? h('code', null, t.slice(1, -1)) : h('strong', null, t.slice(2, -2)))
    last = m.index + t.length
  }
  if (last < line.length) out.push(line.slice(last))
  return out
}

function clip(s, n) {
  const lines = s.replace(/\n+$/, '').split('\n')
  return lines.length <= n ? lines.join('\n') : lines.slice(0, n).join('\n') + `\n… ${lines.length - n} more lines`
}

// ---- Event stream for the open agent

function follow(id) {
  if (S.esFor === id && S.es && S.es.readyState !== EventSource.CLOSED) return
  if (S.es) S.es.close()
  const c = convo(id)
  // Opened for the first time, only the history's end comes: the earlier
  // is fetched as it is scrolled up to.
  const tail = !c.lastID
  const es = new EventSource(`/v1/agents/${encodeURIComponent(id)}/events?follow=1&` + (tail ? `tail=${tailEvents}` : `after=${c.lastID}`))
  es.onmessage = (m) => {
    const e = JSON.parse(m.data)
    apply(c, e)
    // Status rides the same stream; show it now rather than at the next poll.
    // One in the history is long past: the agent's list says what it is now.
    if (e.kind === 'status' && !e.ephemeral && c.ready) {
      const a = agentById(id)
      if (a && a.state?.status !== e.status) {
        a.state = { ...a.state, status: e.status }
        noteTurns()
        renderSidebar()
        if (S.sel === id) renderTopbar()
      }
    }
    // The history is drawn once, when it is all there: drawn as it came,
    // a long one was drawn over and over, each time whole.
    if (S.sel === id && c.ready) scheduleConvo()
  }
  // After its history: an agent with none has none yet.
  es.addEventListener('ready', (m) => {
    if (tail) { try { c.more = !!JSON.parse(m.data).earlier } catch { c.more = false } }
    c.ready = true
    if (S.sel === id) scheduleConvo()
  })
  es.onerror = () => {
    if (es.readyState === EventSource.CLOSED && S.sel === id) setTimeout(() => S.sel === id && follow(id), 2000)
  }
  S.es = es
  S.esFor = id
}

// tailEvents is how many of the last events come at first, earlierEvents
// how many more each time the earlier are scrolled up to; both reach back
// to a message the person sent.
const tailEvents = 400
const earlierEvents = 1500

// loadEarlier fetches the events before the first the conversation has and
// draws them above, the one read staying where it was. It is true when they
// came; failed, the next scroll up tries again.
async function loadEarlier(id) {
  const c = convo(id)
  if (!c.more || c.loadingEarlier) return false
  c.loadingEarlier = true
  if (S.sel === id) scheduleConvo()
  const r = await guarded(() => api('GET', `/v1/agents/${encodeURIComponent(id)}/events?tail=${earlierEvents}&before=${c.first}`))
  c.loadingEarlier = false
  if (r) prepend(c, r.events || [], !!r.earlier)
  if (S.sel === id) showEarlier(c)
  return !!r
}

// prepend puts earlier events in front of a conversation. Those later can
// reach back over them: a decision on an approval, a status that voids the
// open ones. A rewind over them, or one run of a tool on both sides, folds
// it all again instead, keeping only an answer being written, which is not
// stored.
function prepend(c, evs, earlier) {
  c.more = earlier
  if (!evs.length) return
  const before = newConvo()
  for (const e of evs) apply(before, e)
  const crosses = c.log.some((e) => e.kind === 'rewind' && e.rewind && e.rewind.from < c.firstSrc) ||
    [...before.byItem.keys()].some((k) => c.byItem.has(k))
  const log = before.log.concat(c.log)
  // What is drawn starts where it did, now further down.
  const was = groupTools(c.entries).length
  if (crosses) {
    const all = newConvo()
    for (const e of log) apply(all, e)
    for (const en of c.entries) if (en.live) { all.entries.push(en); if (en.item) all.byItem.set(en.item, en) }
    c.entries = all.entries
    c.byItem = all.byItem
  } else {
    for (const e of c.log) if (e.kind === 'approval_done' || e.kind === 'status') apply(before, e)
    c.entries = before.entries.concat(c.entries)
    for (const [k, en] of before.byItem) c.byItem.set(k, en)
  }
  if (c.from !== undefined) c.from += groupTools(c.entries).length - was
  c.log = log
  c.first = evs[0].id
  c.firstSrc = evs[0].src
}

// Coalesce a burst of events into one render. A timer rather than
// requestAnimationFrame: rAF never fires in a hidden tab, and a tab brought
// back should show what happened while it was away.
let convoQueued = false
function scheduleConvo() {
  if (convoQueued) return
  convoQueued = true
  setTimeout(() => { convoQueued = false; renderConvo() }, 30)
}

// ---- Polling

// Answers can cross: only the newest asked for is drawn.
let agentsAsked = 0
async function refreshAgents() {
  const n = ++agentsAsked
  const [agents, order] = await Promise.all([api('GET', '/v1/agents'), api('GET', '/v1/agent-order')])
  if (n !== agentsAsked) return
  const before = agentById(S.sel)
  S.agents = agents
  S.order = order
  noteTurns()
  // The open agent was archived as a new session took its place (/new):
  // go on in the new one, as if it were the same agent.
  const now = agentById(S.sel)
  if (S.view === 'agent' && before && !before.archived_at && now?.archived_at) {
    const next = agents.find((a) => !a.archived_at && a.line === now.line)
    if (next) {
      history.replaceState(null, '', '#/a/' + next.id)
      select(next.id, false)
      return
    }
  }
  if (!S.sel && S.view === 'agent') {
    const live = agents.filter((a) => !a.archived_at)
    if (live.length) select(live[live.length - 1].id, false)
  }
  // A deep link (#/a/<id>) is drawn before the list arrives; draw the agent
  // once it is known.
  if (S.view === 'agent' && S.sel && S.drawn !== S.sel && agentById(S.sel)) { render(); return }
  renderSidebar()
  if (S.view === 'agent') { renderTopbar(); renderStatusLine() }
  // Its turn started or ended between two events of its stream: the dots under its last word follow.
  if (S.view === 'agent' && before && now && before.state?.status !== now.state?.status) scheduleConvo()
}

async function refreshIsolations() {
  S.isolations = await api('GET', '/v1/isolations')
  if (S.view === 'isolations') {
    await refreshExposures()
    if (!editing('isolations')) renderIsolations()
  }
  if (S.view === 'new') S.refillIsolations?.()
  // Reading a CLI starts it inside the isolation: once per visit, not on
  // every poll. Accounts lists every isolation's; an isolation's Logins tab
  // reads its own.
  const logins = S.view === 'setup' ? S.isolations.map((i) => i.name)
    : S.view === 'isolations' && S.isoTab === 'logins' && shownIso() ? [shownIso().name] : []
  if (logins.length && S.accounts == null) await refreshAccounts()
  const missing = logins.filter((n) => !(n in S.clis))
  if (missing.length) await refreshCLIs(missing)
}

// editing is whether redrawing page id now would lose what the operator is
// typing: an inline form is open, or a field on it has the focus.
function editing(id) {
  if (S.openForm) return true
  // Text being selected (to copy an address, say) would be lost to a redraw.
  const sel = window.getSelection && window.getSelection()
  if (sel && !sel.isCollapsed && sel.anchorNode) {
    const n = sel.anchorNode.nodeType === 1 ? sel.anchorNode : sel.anchorNode.parentElement
    if (n && n.closest && n.closest('#' + id)) return true
  }
  const a = document.activeElement
  return !!(a && a.closest && a.closest('#' + id) && (/^(INPUT|SELECT|TEXTAREA)$/.test(a.tagName) || a.classList.contains('dropdown')))
}

async function refreshServices() {
  const [services, envs] = await Promise.all([api('GET', '/v1/services'), api('GET', '/v1/envs')])
  S.services = services || []
  S.envs = envs || []
  // A service's page says which isolations have it.
  if (S.view === 'services') await refreshExposures()
  if (S.view === 'services' && !editing('services')) renderServices()
}

async function refreshExposures() {
  const out = {}
  await Promise.all(S.isolations.map(async (i) => { out[i.name] = await api('GET', `/v1/isolations/${encodeURIComponent(i.name)}/services`) }))
  S.exposures = out
  if (!S.services.length) {
    const [services, envs] = await Promise.all([api('GET', '/v1/services'), api('GET', '/v1/envs')])
    S.services = services || []
    S.envs = envs || []
  }
}

// startPolling keeps the lists current. The daemon names a list on
// /v1/changes each time it changes, and only that one is read again; a
// page drawn anew every two seconds lost a selection being made on it.
// On connecting, and again after a reconnect, every list is read, as one
// may have changed while the stream was down. A hidden tab closes it and
// catches up the moment it is shown again.
function startPolling() {
  let es = null
  let retry = null
  const all = async () => {
    await guarded(refreshAgents)
    await guarded(refreshIsolations)
    if (S.view === 'services') await guarded(refreshServices)
  }
  const on = {
    hello: all,
    agents: () => guarded(refreshAgents),
    isolations: () => guarded(refreshIsolations),
    // The services' envs and which isolations have each.
    services: () => S.view === 'services' ? guarded(refreshServices) : S.view === 'isolations' ? guarded(refreshIsolations) : null,
  }
  const open = () => {
    clearTimeout(retry)
    retry = null
    if (es || document.hidden) return
    if (!S.authed) { retry = setTimeout(open, 2000); return }
    es = new EventSource('/v1/changes')
    for (const [name, fn] of Object.entries(on)) es.addEventListener(name, fn)
    // EventSource reconnects by itself unless the answer was not a stream
    // (signed out, the daemon restarting): then try again in a while.
    es.onerror = () => {
      if (es.readyState !== EventSource.CLOSED) return
      es = null
      retry = setTimeout(open, 2000)
    }
  }
  const close = () => {
    clearTimeout(retry)
    retry = null
    if (es) { es.close(); es = null }
  }
  // The first load always happens, a hidden tab's too.
  if (document.hidden && S.authed) all()
  open()
  document.addEventListener('visibilitychange', () => document.hidden ? close() : open())
}

// ---- Actions

// The address bar mirrors the view (#/a/<id>, #/new, #/isolations), so a
// page can be bookmarked and a phone's back gesture does the expected thing.
function setHash(hash) {
  if (location.hash !== hash) history.pushState(null, '', hash)
}

// noteTurns marks an agent whose turn has ended, working to idle, unless it
// is the one open; one that is working again, parked, archived or gone is no
// longer marked.
function noteTurns() {
  const before = S.unread.size, changed = []
  for (const a of S.agents) {
    // Autocompact's /compact is no turn to look at: its end marks none.
    const st = a.archived_at ? 'archived' : a.compacting && a.state?.status === 'working' ? 'compacting' : a.state?.status || 'idle'
    const was = S.lastStatus.get(a.id)
    S.lastStatus.set(a.id, st)
    if (was === 'working' && st === 'idle' && !(S.view === 'agent' && S.sel === a.id) && !a.parked) { S.unread.add(a.id); changed.push(a.id) }
    else if (S.unread.has(a.id) && ((st !== 'idle' && st !== 'compacting') || a.parked)) { S.unread.delete(a.id); changed.push(a.id) }
  }
  for (const id of S.unread) if (!agentById(id)) { S.unread.delete(id); changed.push(id) }
  if (changed.length) saveUnread()
  let gone = false
  for (const id of S.drafts.keys()) if (!agentById(id)) { S.drafts.delete(id); gone = true }
  if (gone) saveDrafts()
}

function saveUnread() {
  try { localStorage.setItem('psps.unread', JSON.stringify([...S.unread])) } catch {}
}

// setDraft keeps what is typed to an agent, as it is typed; an empty one is
// dropped.
function setDraft(id, text) {
  if (text) S.drafts.set(id, text)
  else if (!S.drafts.delete(id)) return
  saveDrafts()
}

function saveDrafts() {
  try { localStorage.setItem('psps.drafts', JSON.stringify(Object.fromEntries(S.drafts))) } catch {}
}

function select(id, closeDrawer = true) {
  if (S.unread.delete(id)) saveUnread()
  if (S.sel !== id) S.edit = null
  S.sel = id
  S.view = 'agent'
  S.settingsOpen = false
  if (closeDrawer) S.drawer = false
  setHash('#/a/' + id)
  follow(id)
  render()
}

function show(view) {
  setHash({ new: '#/new', isolations: '#/isolations', services: '#/services', setup: '#/setup' }[view] || location.hash)
  route()
}

// go opens a page of the settings: one item of a list, or a tab of it.
function go(hash) {
  setHash(hash)
  route()
}

const NEW = '+new'
const ISO_TABS = ['overview', 'services', 'logins', 'environment', 'settings']
const isoHash = (name, tab) => '#/isolations/' + enc(name) + (tab && tab !== 'overview' ? '/' + tab : '')
const svcHash = (name) => '#/services/' + enc(name)
const setupHash = (kind, name) => '#/setup/' + kind + (name ? '/' + enc(name) : '')

function route() {
  const was = S.view
  const hash = location.hash
  let m
  if ((m = hash.match(/^#\/a\/([a-z0-9-]+)$/))) { S.sel = m[1]; S.view = 'agent'; follow(m[1]) }
  else if (hash === '#/new') S.view = 'new'
  else if ((m = hash.match(/^#\/isolations(?:\/([^/]+)(?:\/([a-z]+))?)?$/))) {
    S.view = 'isolations'
    S.isoSel = m[1] ? decodeURIComponent(m[1]) : null
    S.isoTab = ISO_TABS.includes(m[2]) ? m[2] : 'overview'
  } else if ((m = hash.match(/^#\/services(?:\/([^/]+))?$/))) {
    S.view = 'services'
    S.svcSel = m[1] ? decodeURIComponent(m[1]) : null
  } else if ((m = hash.match(/^#\/setup(?:\/(iso|account|new)(?:\/([^/]+))?)?$/))) {
    S.view = 'setup'
    S.setupSel = m[1] ? { kind: m[1], name: m[2] ? decodeURIComponent(m[2]) : null } : null
  } else S.view = 'agent'
  // A form belongs to what was on screen when it was opened.
  if (was !== S.view || hash !== S.routed) { S.openForm = null; S.verifyOut = null }
  S.routed = hash
  S.drawer = false
  S.settingsOpen = false
  // Accounts reads each isolation's CLIs once per visit, from inside it.
  if (S.view === 'setup' && was !== 'setup') { S.clis = {}; S.accounts = null }
  if (!(S.view === 'setup' && S.setupSel?.kind === 'new')) S.acctDraft = { name: '', kind: null, secret: '' }
  if (S.view === 'isolations' && S.isoTab === 'logins' && was !== 'isolations') S.clis = {}
  render()
  // A new agent's form starts at its first message, the rest being as last
  // time; not on a phone, where the keyboard would cover the form.
  if (S.view === 'new' && was !== 'new' && !coarse) document.getElementById('new-prompt')?.focus()
  if (S.view !== 'agent') guarded(refreshIsolations)
  if (S.view === 'services') guarded(refreshServices)
  document.getElementById('main')?.querySelector('.page')?.scrollTo?.(0, 0)
}
window.addEventListener('popstate', route)

async function send(text) {
  const id = S.sel
  await guarded(async () => {
    // One of psps's own commands is done, not sent, and says what it did.
    const r = await api('POST', `/v1/agents/${id}/messages`, { text })
    if (r.done) {
      toast(r.done)
      await refreshAgents()
    }
  })
}

async function decide(approvalID, decision, answers) {
  await guarded(() => api('POST', `/v1/agents/${S.sel}/approvals/${encodeURIComponent(approvalID)}`, { decision, answers }))
}

// ---- Sidebar width: its edge is dragged, by mouse or touch, and the
// width is kept in this browser. A double click puts it back.

const sidebarDefault = 290
function setSidebarWidth(w) {
  if (w == null) { root.style.removeProperty('--sidebar-w'); return }
  w = Math.round(Math.min(Math.max(w, 200), Math.min(640, window.innerWidth * 0.6)))
  root.style.setProperty('--sidebar-w', w + 'px')
  return w
}
try { const w = +localStorage.getItem('psps.sidebarWidth'); if (w) setSidebarWidth(w) } catch {}

function sidebarResizer() {
  const bar = h('div', { class: 'sidebar-resizer', role: 'separator', 'aria-orientation': 'vertical', title: 'Drag to resize · double-click to reset' })
  let startX = 0, startW = 0, w = null
  const move = (e) => {
    w = setSidebarWidth(startW + e.clientX - startX)
    fitTopbar()
  }
  const end = () => {
    bar.classList.remove('dragging')
    document.body.classList.remove('resizing')
    bar.removeEventListener('pointermove', move)
    try { if (w) localStorage.setItem('psps.sidebarWidth', String(w)) } catch {}
  }
  bar.addEventListener('pointerdown', (e) => {
    if (e.button !== 0) return
    e.preventDefault()
    startX = e.clientX
    startW = document.getElementById('sidebar').getBoundingClientRect().width
    w = null
    bar.setPointerCapture(e.pointerId)
    bar.classList.add('dragging')
    document.body.classList.add('resizing')
    bar.addEventListener('pointermove', move)
  })
  bar.addEventListener('pointerup', end)
  bar.addEventListener('pointercancel', end)
  bar.addEventListener('dblclick', () => {
    setSidebarWidth(null)
    try { localStorage.removeItem('psps.sidebarWidth') } catch {}
    fitTopbar()
  })
  return bar
}

// ---- Rendering

function render() {
  root.className = 'app' + (S.drawer ? ' drawer' : '')
  // The page is drawn anew, the sidebar's list where it was scrolled.
  const scrolled = document.querySelector('#sidebar .agent-list')?.scrollTop || 0
  root.replaceChildren()
  if (!S.authed) {
    root.style.display = 'block'
    root.append(loginView())
    return
  }
  root.style.display = ''
  root.append(h('aside', { class: 'sidebar', id: 'sidebar' }), sidebarResizer(), h('div', { class: 'scrim', onclick: () => { S.drawer = false; render() } }))
  const main = h('main', { class: 'main', id: 'main' })
  root.append(main)
  renderSidebar()
  const list = document.querySelector('#sidebar .agent-list')
  if (list && scrolled) list.scrollTop = scrolled
  if (S.view === 'new') main.append(topbar('New agent'), newAgentView())
  else if (S.view === 'isolations') main.append(topbar('Isolations'), h('div', { class: 'page split', id: 'isolations' }))
  else if (S.view === 'services') main.append(topbar('Services'), h('div', { class: 'page split', id: 'services' }))
  else if (S.view === 'setup') main.append(topbar('Accounts'), h('div', { class: 'page split', id: 'setup' }))
  else main.append(agentView())
  if (S.view === 'isolations') renderIsolations()
  if (S.view === 'services') renderServices()
  if (S.view === 'setup') renderSetup()
  renderToast()
}

function topbar(title, sub, ...extra) {
  return h('header', { class: 'topbar', id: 'topbar' },
    h('button', { class: 'menu-btn', 'aria-label': 'Agents', onclick: () => { S.drawer = true; render() } }, icon('menu')),
    h('div', { class: 'title' }, h('h1', null, title), sub ? h('div', { class: 'sub' }, sub) : null),
    ...extra)
}

// togglePark parks an idle agent, or takes a parked one back: it is idle
// and wants a look again, as it does once it next does anything.
function togglePark(a) {
  if (a.archived_at || (a.state?.status || 'idle') !== 'idle') return false
  const want = !a.parked
  a.parked = want
  if (want && S.unread.delete(a.id)) saveUnread()
  renderSidebar()
  guarded(async () => {
    try {
      await api('PATCH', `/v1/agents/${a.id}`, { parked: want })
    } catch (x) {
      a.parked = !want
      renderSidebar()
      throw x
    }
  })
  return true
}

// msgAge is how long ago t was, in one word: now, 1m … 59m, 1h … 23h, 1d …
// 364d, 1y, 2y, rounded down.
function msgAge(t) {
  const m = Math.floor((Date.now() - new Date(t).getTime()) / 60000)
  if (!(m >= 1)) return 'now'
  if (m < 60) return m + 'm'
  const hr = Math.floor(m / 60)
  if (hr < 24) return hr + 'h'
  const d = Math.floor(hr / 24)
  return d < 365 ? d + 'd' : Math.floor(d / 365) + 'y'
}

// nameWithAge is a sidebar row's name, with how long ago its last message
// was at its end. An agent unread has that in red, where one with no
// message yet has the red dot.
function nameWithAge(a, mark) {
  const unread = S.unread.has(a.id)
  const at = a.last_message_at
  if (!unread && !at) return h('span', { class: 'name' }, mark, agentTitle(a))
  const said = at ? `Last message ${new Date(at).toLocaleString()}` : ''
  return h('span', { class: 'name split' }, h('span', { class: 'title' }, mark, agentTitle(a)),
    at ? h('span', { class: 'age' + (unread ? ' unread' : ''), 'data-at': at,
      title: unread ? `Its turn ended; not opened since. ${said}` : said }, msgAge(at))
      : h('span', { class: 'unread-dot', title: 'Its turn ended; not opened since', 'aria-label': 'turn ended, not opened since' }))
}

// The ages count on once a minute, in place.
setInterval(() => {
  for (const el of document.querySelectorAll('#sidebar .age[data-at]')) {
    const now = msgAge(el.dataset.at)
    if (el.textContent !== now) el.textContent = now
  }
}, 60000)

function renderSidebar() {
  const el = document.getElementById('sidebar')
  if (!el) return
  if (S.sorting || S.picking) { S.sidebarStale = true; return }
  S.sidebarStale = false
  const live = S.agents.filter((a) => !a.archived_at && !a.host)
  const admins = S.agents.filter((a) => !a.archived_at && a.host)
  const archived = S.agents.filter((a) => a.archived_at)
  const row = (a) => {
    const live = a.archived_at ? 'idle' : a.state?.status || 'idle'
    const st = live === 'idle' && a.parked ? 'parked' : live
    // An idle agent's dot parks it, a parked one's takes it back: it is
    // idle and wants a look again, as it does once it next does anything.
    const togglable = !a.archived_at && live === 'idle'
    const dot = pickLead(a, st, togglable)
    const meta = a.problem ? h('span', { class: 'problem' }, icon('triangle-alert'), ' ' + a.problem)
      : [`${shownID(a)} · ${a.isolation}`, a.inbox ? [' · ', icon('mail'), ` ${a.inbox}`] : null]
    // What it runs beside its turn, as icons alone at the line's end.
    const tasks = liveTasks(a)
    const marks = tasks.length ? h('span', { class: 'tasks-marks', title: tasksSummary(tasks), 'aria-label': tasksSummary(tasks) }, ...taskMarks(tasks, false)) : null
    // A team's lead wears a crown; an agent it made, an arrow from it, as
    // does one an agent made through the control service.
    const maker = a.made_via && S.agents.find((x) => x.id === a.made_via)
    const mark = a.leads ? h('span', { class: 'mark', title: 'Leads its team' }, icon('crown'))
      : a.made_by ? h('span', { class: 'mark', title: "Made by its team's lead" }, icon('corner-down-right'))
      : a.made_via ? h('span', { class: 'mark', title: `Made by agent ${maker ? shownID(maker) : a.made_via}, through the control service` }, icon('corner-down-right')) : null
    return h('button', {
      class: 'agent' + (a.id === S.sel && S.view === 'agent' ? ' selected' : '') + (a.archived_at ? ' archived' : ''),
      'data-line': a.line || a.id, 'data-agent': a.id, 'data-group': a.archived_at ? 'archived' : 'live', onclick: () => select(a.id),
    }, dot, nameWithAge(a, mark),
      h('span', { class: 'meta with-marks' }, h('span', { class: 'meta-text' }, meta), marks, iconPick(a)))
  }
  // An agent's archived sessions are one folded line, under the ID it goes
  // by now; a lone one is a row of its own.
  const archivedRows = () => archivedEntries(archived).map((e) => {
    if (e.sessions.length === 1) return row(e.sessions[0])
    const open = S.openLines.has(e.line)
    const current = S.agents.find((a) => !a.archived_at && a.line === e.line) || e.sessions[e.sessions.length - 1]
    return h('div', { class: 'line-group' + (open ? ' open' : ''), 'data-line': e.line },
      h('button', {
        class: 'agent line-head', 'aria-expanded': String(open),
        onclick: () => { open ? S.openLines.delete(e.line) : S.openLines.add(e.line); renderSidebar() },
      }, h('span', { class: 'caret' }, icon(open ? 'chevron-down' : 'chevron-right')), h('span', { class: 'name' }, shownID(current)),
      h('span', { class: 'meta' }, `${e.sessions.length} sessions · ${current.isolation}`)),
      open ? h('div', { class: 'line-sessions' }, e.sessions.map(row)) : null)
  })
  // Polling redraws the sidebar: the list stays where it was scrolled.
  const scrolled = el.querySelector('.agent-list')?.scrollTop || 0
  el.replaceChildren(
    h('div', { class: 'brand' }, 'psps', h('span', { class: 'count' }, `${live.length} agent${live.length === 1 ? '' : 's'}`),
      h('button', { class: 'add-divider', title: 'Add a divider at the bottom; hold it to move it', 'aria-label': 'Add a divider', onclick: addDivider },
        icon('separator-horizontal')),
      h('button', { class: 'brand-settings', title: 'Settings', 'aria-label': 'Settings', onclick: openSettings }, icon('settings'))),
    ...admins.map(adminRow),
    h('button', { class: 'new-agent' + (S.view === 'new' ? ' active' : ''), onclick: () => show('new') }, '+ New agent'),
    h('div', { class: 'agent-list' },
      sortable('live', liveRows(live, row)),
      h('button', { class: 'fold', onclick: () => { S.showArchived = !S.showArchived; renderSidebar() } },
        icon(S.showArchived ? 'chevron-down' : 'chevron-right'), ` Archived (${archived.length})`),
      S.showArchived ? h('div', { class: 'archived-list' }, archivedRows()) : null),
    h('div', { class: 'side-bottom' }),
  )
  // What was picked and is out of sight now, folded away or gone, is let go.
  if (S.pick) {
    const shown = new Set(pickIDs(S.pick.group))
    for (const id of S.pick.sel) if (!shown.has(id)) S.pick.sel.delete(id)
    if (!shown.has(S.pick.last)) S.pick.last = null
  }
  paintPick()
  // Scrolled back once the foot paintPick puts in is there: before, the
  // list was the foot's height taller, and one scrolled to its end came
  // back short of it by that much.
  if (scrolled) el.querySelector('.agent-list').scrollTop = scrolled
  placeIconPick()
}

function sideFoot() {
  return h('div', { class: 'side-foot' },
    h('button', { class: S.view === 'isolations' ? 'active' : '', onclick: () => show('isolations') }, icon('box'), ' Isolations'),
    h('button', { class: S.view === 'services' ? 'active' : '', onclick: () => show('services') }, icon('key-round'), ' Services'),
    h('button', { class: S.view === 'setup' ? 'active' : '', onclick: () => show('setup') }, icon('user-round'), ' Accounts'))
}

// ---- Picking agents in the sidebar, to archive, restore or delete them at
// once. A long press on a row's status icon starts it: the row is picked,
// and the icons of its group (the live agents, or the archived) become
// boxes. Dragging on from the press, up or down, does to every row it
// passes what the press did to its own, picking or letting go; a row the
// drag goes back over is as it was. Once picking, a press on a box does
// this at once, and a Shift-click carries the last press on from its row
// to the clicked one. A press on the other group's icon moves the picking
// there, what was picked let go. Rows are picked by their icon alone; the
// row opens its agent as ever. Esc or the bar's x stops.

// pickLead is a row's status icon, with the box it becomes, in a cell a
// finger can find: a tap on an idle or parked agent's parks it or takes it
// back, a long press starts picking.
function pickLead(a, st, togglable) {
  const dot = h('span', {
    class: 'dot ' + st + (togglable ? ' toggle' : ''), role: togglable ? 'button' : 'img', 'aria-label': st,
    title: (!togglable ? st : st === 'parked' ? 'parked: needs no look; tap to make it idle' : 'idle: wants a look; tap to park it') + ' · hold to pick',
  })
  const box = h('span', { class: 'pick-box' + (a.archived_at ? '' : ' ' + st), title: 'Click to pick · drag or Shift-click for a range' }, icon('check'))
  const lead = h('span', { class: 'lead' }, dot, box)
  lead.addEventListener('pointerdown', (e) => pickDown(e, a))
  lead.addEventListener('click', (e) => {
    const row = lead.closest('.agent')
    if (S.pick?.group === row?.dataset.group) { e.preventDefault(); e.stopPropagation(); return }
    if (togglable) { e.stopPropagation(); togglePark(a) }
  })
  return lead
}

// pickIDs are the agents of the group whose rows are in the sidebar, top
// to bottom: those a range runs over.
const pickIDs = (group) => [...document.querySelectorAll(`#sidebar .agent[data-group="${group}"]`)].map((r) => r.dataset.agent)

// The press going on: where it started, the long press's timer, and once
// it ranges, over which rows (ids), from which (anchor) to which (cur),
// picking or letting go (on), over what was picked before it (before).
let pickPress = null
// The click a range's press ends in, on whichever row it was let go, is
// not a click: the next one is swallowed, unless another press comes first.
let pickSwallow = false

function pickDown(e, a) {
  if (e.button !== 0) return
  const row = e.currentTarget.closest('.agent'), group = row?.dataset.group
  if (!group) return
  // Not the row's: the sortable list's own long press moves it.
  e.stopPropagation()
  e.preventDefault()
  pickRelease()
  const P = S.pick
  if (P && P.group === group) {
    if (e.shiftKey && e.pointerType === 'mouse' && P.last) {
      const ids = pickIDs(group), from = ids.indexOf(P.last), to = ids.indexOf(a.id)
      if (from >= 0 && to >= 0) {
        pickApply(ids, from, to, P.lastOn, new Set(P.sel))
        P.last = a.id
        paintPick()
        return
      }
    }
    pickPress = { id: e.pointerId, x: e.clientX, y0: e.clientY, group }
    S.picking = true
    pickRange(a.id)
    return
  }
  pickPress = { id: e.pointerId, x: e.clientX, y0: e.clientY, group, agent: a.id }
  S.picking = true
  pickPress.timer = setTimeout(() => {
    pickPress.timer = null
    navigator.vibrate?.(10)
    S.pick = { group, sel: new Set(), last: null, lastOn: true, confirm: false, purge: false }
    pickRange(a.id)
  }, 450)
  paintPick()
}

// pickRange starts a range at the agent's row: it is picked, or let go if
// it was, and so is each row the press is dragged over.
function pickRange(id) {
  const P = S.pick, ids = pickIDs(P.group), at = ids.indexOf(id)
  if (at < 0) return
  const on = !P.sel.has(id)
  pickPress.range = { ids, anchor: at, cur: at, on, before: new Set(P.sel) }
  P.last = id
  P.lastOn = on
  pickApply(ids, at, at, on, pickPress.range.before)
  pickPress.frame = requestAnimationFrame(pickScroll)
  paintPick()
}

function pickApply(ids, from, to, on, before) {
  const sel = new Set(before)
  for (let i = Math.min(from, to); i <= Math.max(from, to); i++) on ? sel.add(ids[i]) : sel.delete(ids[i])
  S.pick.sel = sel
  S.pick.confirm = false
}

// pickOver ranges to the row of its group at the press's height, however
// far the finger strays sideways or a team's rows are set in.
function pickOver(y) {
  const r = pickPress?.range
  if (!r) return
  const row = [...document.querySelectorAll(`#sidebar .agent[data-group="${pickPress.group}"]`)].find((el) => {
    const b = el.getBoundingClientRect()
    return y >= b.top && y < b.bottom
  })
  if (!row) return
  const at = r.ids.indexOf(row.dataset.agent)
  if (at < 0 || at === r.cur) return
  r.cur = at
  pickApply(r.ids, r.anchor, at, r.on, r.before)
  paintPick()
}

// Held near the list's top or bottom edge, the list scrolls, and the range
// goes on over what comes into view.
function pickScroll() {
  if (!pickPress?.range) return
  const list = document.querySelector('#sidebar .agent-list')
  if (list && pickPress.y != null) {
    const b = list.getBoundingClientRect(), edge = 40
    const step = pickPress.y < b.top + edge ? -8 : pickPress.y > b.bottom - edge ? 8 : 0
    if (step) { list.scrollTop += step; pickOver(Math.min(Math.max(pickPress.y, b.top + 1), b.bottom - 1)) }
  }
  pickPress.frame = requestAnimationFrame(pickScroll)
}

// pickRelease ends the press: a long press let go before its time is a tap.
function pickRelease() {
  if (!pickPress) return
  clearTimeout(pickPress.timer)
  cancelAnimationFrame(pickPress.frame)
  if (pickPress.range) pickSwallow = true
  pickPress = null
  S.picking = false
  if (S.sidebarStale) renderSidebar()
  else paintPick()
}

addEventListener('pointermove', (e) => {
  if (!pickPress || e.pointerId !== pickPress.id) return
  pickPress.y = e.clientY
  // Moved before the long press is up: it scrolls, or was nothing.
  if (pickPress.timer) { if (Math.hypot(e.clientX - pickPress.x, e.clientY - pickPress.y0) > 8) pickRelease(); return }
  pickOver(e.clientY)
})
addEventListener('pointerup', (e) => { if (pickPress && e.pointerId === pickPress.id) pickRelease() })
addEventListener('pointercancel', (e) => { if (pickPress && e.pointerId === pickPress.id) pickRelease() })
// A range is not scrolled with, nor given the long press's menu.
addEventListener('touchmove', (e) => { if (pickPress?.range) e.preventDefault() }, { passive: false })
addEventListener('contextmenu', (e) => { if (pickPress) e.preventDefault() })
document.addEventListener('pointerdown', () => { pickSwallow = false }, true)
document.addEventListener('click', (e) => {
  if (!pickSwallow) return
  pickSwallow = false
  e.preventDefault()
  e.stopPropagation()
}, true)

// paintPick shows what is picked on the rows as they are, and the bar for
// it at the sidebar's foot, without drawing the list anew under a press.
function paintPick() {
  const P = S.pick, r = pickPress?.range
  for (const row of document.querySelectorAll('#sidebar .agent[data-group]')) {
    const id = row.dataset.agent, on = !!P && P.group === row.dataset.group
    const at = r && on ? r.ids.indexOf(id) : -1
    row.classList.toggle('picking', on)
    row.classList.toggle('pick-other', !!P && !on)
    row.classList.toggle('picked', on && P.sel.has(id))
    row.classList.toggle('pick-last', on && P.last === id)
    row.classList.toggle('in-range', at >= 0 && at >= Math.min(r.anchor, r.cur) && at <= Math.max(r.anchor, r.cur))
    row.classList.toggle('holding', !!pickPress?.timer && pickPress.agent === id)
  }
  // The heads of the other group's folds fade with its rows.
  for (const head of document.querySelectorAll('#sidebar .line-head')) head.classList.toggle('pick-other', P?.group === 'live')
  for (const head of document.querySelectorAll('#sidebar .team-head')) head.classList.toggle('pick-other', P?.group === 'archived')
  document.querySelector('#sidebar .side-bottom')?.replaceChildren(P ? pickBar() : sideFoot())
}

function stopPicking() {
  S.pick = null
  paintPick()
}

function pickBar() {
  const P = S.pick, n = P.sel.size, archived = P.group === 'archived'
  const ids = pickIDs(P.group), all = ids.length > 0 && ids.every((id) => P.sel.has(id))
  const head = h('div', { class: 'pick-head' },
    h('button', { class: 'pick-x', title: 'Stop picking (Esc)', 'aria-label': 'Stop picking', onclick: stopPicking }, icon('x')),
    h('span', { class: 'pick-count', 'aria-live': 'polite' }, `${n} picked`, h('small', null, archived ? ' in Archived' : ' in Active')),
    h('button', {
      class: 'pick-all', disabled: !ids.length || P.busy,
      onclick: () => { P.sel = all ? new Set() : new Set(ids); P.confirm = false; paintPick() },
    }, all ? 'None' : 'All'))
  const act = (cls, ic, label, onclick, disabled = !n) =>
    h('button', { class: 'btn pick-act ' + cls, disabled: disabled || P.busy, onclick }, icon(ic), ' ' + label)
  if (!archived) return h('div', { class: 'pick-bar' }, head, h('div', { class: 'pick-acts' }, act('primary', 'archive', 'Archive', pickArchive)))
  if (P.confirm) {
    const purge = h('input', { type: 'checkbox', checked: P.purge, onchange: () => { P.purge = purge.checked } })
    return h('div', { class: 'pick-bar' }, head,
      h('div', { class: 'pick-confirm' }, `Delete ${n} archived session${n === 1 ? '' : 's'}? Their history and logs are removed. This cannot be undone.`),
      h('label', { class: 'check pick-purge' }, purge, 'Also remove their workspaces and session transcripts'),
      h('div', { class: 'pick-acts' },
        h('button', { class: 'btn pick-act', disabled: P.busy, onclick: () => { P.confirm = false; paintPick() } }, 'Cancel'),
        act('danger-fill', 'trash-2', 'Delete', pickDelete)))
  }
  return h('div', { class: 'pick-bar' }, head, h('div', { class: 'pick-acts' },
    act('', 'archive-restore', 'Restore', pickRestore),
    act('danger', 'trash-2', 'Delete', () => { P.confirm = true; paintPick() })))
}

// pickDo does to each picked agent in turn, and says how it went: it goes
// on past one that fails, and says why the first did. An agent open among
// them that is no longer in the list it was in (gone) is closed.
async function pickDo(fn, did, gone) {
  const P = S.pick, ids = [...P.sel]
  P.busy = true
  paintPick()
  let done = 0, failed = 0, why = ''
  for (const id of ids) {
    try { await fn(id); done++ } catch (x) {
      if (x instanceof Unauthorized) { S.authed = false; render(); return }
      failed++
      why ||= x.message
    }
  }
  S.pick = null
  if (gone && ids.includes(S.sel)) S.sel = null
  toast(failed ? `${did} ${done} of ${ids.length}; ${failed} failed: ${why}` : `${did} ${done}`, !!failed)
  await guarded(refreshAgents)
  render()
}

function pickArchive() {
  return pickDo((id) => api('POST', `/v1/agents/${enc(id)}/archive`), 'Archived', true)
}

// A line carries on as one session: restoring two of a line's would leave
// the first archived again by the second, and restoring one whose line has
// a live session archives that one.
async function pickRestore() {
  const picked = [...S.pick.sel].map(agentById).filter(Boolean)
  const lines = new Map()
  for (const a of picked) lines.set(a.line || a.id, [...(lines.get(a.line || a.id) || []), a])
  const twice = [...lines.values()].filter((ss) => ss.length > 1)
  if (twice.length) {
    // Named as its folded line is in the sidebar.
    const named = (a) => document.querySelector(`#sidebar .line-group[data-line="${CSS.escape(a.line)}"] .line-head .name`)?.textContent || shownID(a)
    toast(`Pick one session of each line to restore: ${twice.map((ss) => `${ss.length} of ${named(ss[0])}`).join(', ')}`, true)
    return
  }
  const taking = picked.filter((a) => S.agents.some((o) => !o.archived_at && o.line && o.line === a.line))
  if (taking.length && !await ask({
    title: `Restore ${picked.length} session${picked.length === 1 ? '' : 's'}?`,
    body: `${taking.length === 1 ? 'One takes' : `${taking.length} take`} the place of the session on ${taking.length === 1 ? 'its line' : 'their lines'} now, which is archived; its crons and waiting mail move over.`,
    ok: 'Restore',
  })) return
  await pickDo((id) => api('POST', `/v1/agents/${enc(id)}/restore`), 'Restored')
}

function pickDelete() {
  const purge = S.pick.purge
  return pickDo((id) => api('DELETE', `/v1/agents/${enc(id)}` + (purge ? '?purge=1' : '')), purge ? 'Deleted, with workspaces,' : 'Deleted', true)
}

// ---- A line's icon

// The icons a line's icon is picked from, and the colors it may be drawn in,
// '' for the text's own; the daemon keeps which one a line has.
const PICK_ICONS = ['star', 'heart', 'flag', 'bookmark', 'pin', 'tag', 'target', 'trophy', 'zap', 'flame', 'rocket', 'sparkles',
  'lightbulb', 'gem', 'bug', 'flask-conical', 'wrench', 'hammer', 'puzzle', 'cpu', 'database', 'server', 'cloud', 'globe',
  'git-branch', 'package', 'file-text', 'book-open', 'chart-line', 'calendar', 'shield', 'lock', 'eye', 'coffee', 'leaf',
  'anchor', 'music', 'smile']
const ICON_COLORS = ['', 'red', 'orange', 'yellow', 'green', 'teal', 'blue', 'violet', 'pink']
const iconColor = (c) => `var(--ic-${c || 'default'})`
const shownIcon = (a) => a.icon && icons[a.icon.name] ? a.icon : null
// styled sets el's CSS properties through the CSSOM: the page's policy
// refuses style attributes.
const styled = (el, props) => { for (const [k, v] of Object.entries(props)) el.style.setProperty(k, v); return el }

// iconPick is the end of a row's second line: the line's icon, or a faint
// button for one, either opening the picker.
function iconPick(a) {
  const ic = shownIcon(a)
  const el = h('span', {
    class: 'pick ' + (ic ? 'set' : 'empty') + (S.iconFresh === a.id ? ' fresh' : ''), role: 'button', tabindex: '0', 'data-pick': a.id,
    'aria-haspopup': 'dialog', 'aria-expanded': String(S.iconFor === a.id),
    title: ic ? 'Change its icon' : 'Give it an icon', 'aria-label': ic ? `Icon: ${ic.name}; change it` : 'Give it an icon',
  }, icon(ic ? ic.name : 'smile-plus'))
  if (ic) styled(el, { color: iconColor(ic.color) })
  // Not the row's: it opens no agent.
  const go = (e) => { e.stopPropagation(); e.preventDefault(); S.iconFor === a.id ? closeIconPick(true) : openIconPick(a) }
  el.addEventListener('click', go)
  el.addEventListener('keydown', (e) => { if (e.key === 'Enter' || e.key === ' ') go(e) })
  el.addEventListener('pointerdown', (e) => e.stopPropagation())
  return el
}

function openIconPick(a) {
  S.iconFor = a.id
  S.iconColor = shownIcon(a)?.color ?? S.iconColor
  renderSidebar()
  buildIconPick()
  const pop = document.getElementById('icon-pop')
  ;(pop?.querySelector('.all [aria-checked="true"]') || pop?.querySelector('.all button'))?.focus()
}

// closeIconPick says whether it was open; back, focus goes to the row's
// icon.
function closeIconPick(refocus) {
  const id = S.iconFor
  if (!id) return false
  S.iconFor = null
  document.getElementById('icon-pop')?.remove()
  renderSidebar()
  if (refocus) document.querySelector(`#sidebar [data-pick="${CSS.escape(id)}"]`)?.focus()
  return true
}

// buildIconPick draws the picker: colors, then the icons picked lately as
// they were, then them all in the color chosen.
function buildIconPick() {
  const a = S.agents.find((x) => x.id === S.iconFor)
  if (!a) return
  let pop = document.getElementById('icon-pop')
  if (!pop) {
    pop = h('div', { id: 'icon-pop', class: 'icon-pop', role: 'dialog' })
    pop.addEventListener('click', (e) => e.stopPropagation())
    document.body.append(pop)
  }
  const ic = shownIcon(a)
  pop.setAttribute('aria-label', `Icon for ${agentTitle(a)}`)
  pop.style.setProperty('--ic', iconColor(S.iconColor))
  const recent = S.recentIcons.filter((r) => icons[r.name] && ICON_COLORS.includes(r.color ?? ''))
  // replaceChildren takes nodes, not lists: these are flattened.
  pop.replaceChildren(...[
    h('div', { class: 'pop-head' }, h('span', { class: 'who' }, agentTitle(a)),
      ic ? h('button', { type: 'button', class: 'clear', onclick: () => setLineIcon(a, null) }, icon('x'), 'Remove') : null),
    h('div', { class: 'swatches', role: 'radiogroup', 'aria-label': 'Color' }, ICON_COLORS.map((c) => styled(h('button', {
      type: 'button', class: 'swatch', role: 'radio', 'aria-checked': String(c === S.iconColor), 'aria-label': c || 'default', title: c || 'default',
      'data-c': c,
      onclick: () => {
        S.iconColor = c
        if (ic) setLineIcon(a, { name: ic.name, color: c }, true)
        else buildIconPick()
        pop.querySelector(`.swatch[data-c="${c}"]`)?.focus()
      },
    }), { '--sw': iconColor(c) }))),
    recent.length ? [h('div', { class: 'pop-label' }, 'Recent'),
      h('div', { class: 'icons recent', role: 'group', 'aria-label': 'Picked lately' }, recent.map((r) => styled(h('button', {
        type: 'button', 'aria-pressed': String(ic?.name === r.name && (ic?.color || '') === (r.color || '')),
        'aria-label': `${r.name}, ${r.color || 'default'}`, title: `${r.name} · ${r.color || 'default'}`,
        onclick: () => setLineIcon(a, { name: r.name, color: r.color || '' }),
      }, icon(r.name)), { color: iconColor(r.color) })))] : null,
    h('div', { class: 'icons all', role: 'radiogroup', 'aria-label': 'Icon' }, PICK_ICONS.map((n) => h('button', {
      type: 'button', role: 'radio', 'aria-checked': String(ic?.name === n), 'aria-label': n, title: n,
      onclick: () => setLineIcon(a, { name: n, color: S.iconColor }),
    }, icon(n)))),
  ].flat().filter(Boolean))
  placeIconPick()
}

// placeIconPick keeps the picker under its row's icon, or over it where
// there is no room below, within the window; with the row gone it closes.
function placeIconPick() {
  const pop = document.getElementById('icon-pop')
  if (!pop) return
  const anchor = S.iconFor && document.querySelector(`#sidebar [data-pick="${CSS.escape(S.iconFor)}"]`)
  if (!anchor) { S.iconFor = null; pop.remove(); return }
  const r = anchor.getBoundingClientRect(), w = pop.offsetWidth, ht = pop.offsetHeight
  const left = Math.max(8, Math.min(r.right + 6 - w, innerWidth - w - 8))
  const below = r.bottom + 6
  pop.style.left = left + 'px'
  pop.style.top = (below + ht > innerHeight - 8 && r.top - 6 - ht >= 8 ? r.top - 6 - ht : below) + 'px'
}

// setLineIcon gives a's line, every session of it, the icon, or with none
// takes its icon away; picking one closes the picker, a new color for the
// icon it has keeps it open.
function setLineIcon(a, ic, keepOpen) {
  const line = a.line || a.id
  const was = a.icon
  const set = (v) => { for (const x of S.agents) if ((x.line || x.id) === line) x.icon = v || undefined }
  set(ic)
  if (ic) {
    // Recent holds it as it is drawn: a new color for the icon it has takes
    // the place of the one it had, if that one is the latest.
    const same = (r, i) => r.name === i.name && (r.color || '') === (i.color || '')
    const rest = S.recentIcons.filter((r) => !same(r, ic))
    if (keepOpen && was && rest[0] && same(rest[0], was)) rest.shift()
    S.recentIcons = [ic, ...rest].slice(0, 8)
    try { localStorage.setItem('psps.recentIcons', JSON.stringify(S.recentIcons)) } catch {}
  }
  if (ic && !keepOpen) {
    S.iconFresh = a.id
    setTimeout(() => { if (S.iconFresh === a.id) S.iconFresh = null }, 300)
  }
  if (keepOpen) { renderSidebar(); buildIconPick() } else closeIconPick(true)
  // A list asked for before the daemon has it would put the old one back:
  // it is dropped, as is one asked for meanwhile.
  agentsAsked++
  guarded(async () => {
    try {
      await api('PATCH', `/v1/agents/${a.id}`, { icon: ic || { name: '' } })
      agentsAsked++
    } catch (x) {
      set(was)
      renderSidebar()
      if (S.iconFor === a.id) buildIconPick()
      throw x
    }
  })
}

// sortable is a sidebar list whose rows, a line each, move by a long press
// (a long click with a mouse) and a drag, within the list only: a team is
// one row of the live list, its agents rows of its own list alone. The
// order is kept by the daemon, so every page has it.
function sortable(list, items) {
  const box = h('div', { class: 'sortable' }, items)
  const scroller = () => box.closest('.agent-list')
  let hold = null, drag = null
  const cancel = () => {
    if (!hold) return
    clearTimeout(hold.timer)
    hold = null
    S.sorting = false
    // After the click a short press is, which opens the row.
    if (S.sidebarStale) setTimeout(renderSidebar)
  }
  const lines = () => [...box.children].map((c) => c.dataset.line)
  // A row moves past another once the pointer is over that one's farther
  // half, so rows of any height do not swap back and forth.
  const place = (y) => {
    const kids = [...box.children]
    const at = kids.indexOf(drag.item)
    for (const [i, c] of kids.entries()) {
      if (c === drag.item) continue
      const r = c.getBoundingClientRect()
      if (y < r.top || y > r.bottom) continue
      const mid = r.top + r.height / 2
      if (i < at && y < mid) box.insertBefore(drag.item, c)
      else if (i > at && y > mid) box.insertBefore(drag.item, c.nextSibling)
      return
    }
  }
  // Held near the list's top or bottom edge, the list scrolls.
  const scroll = () => {
    if (!drag) return
    const sc = scroller(), r = sc.getBoundingClientRect(), edge = 40
    const step = drag.y < r.top + edge ? -8 : drag.y > r.bottom - edge ? 8 : 0
    if (step) { sc.scrollTop += step; place(drag.y) }
    drag.frame = requestAnimationFrame(scroll)
  }
  const start = (item, id, y) => {
    hold = null
    drag = { item, id, y, before: lines().join('\n') }
    item.classList.add('dragging')
    box.classList.add('sorting')
    try { box.setPointerCapture(id) } catch {}
    navigator.vibrate?.(10)
    drag.frame = requestAnimationFrame(scroll)
  }
  const end = (e) => {
    cancel()
    if (!drag || e.pointerId !== drag.id) return
    cancelAnimationFrame(drag.frame)
    drag.item.classList.remove('dragging')
    box.classList.remove('sorting')
    const order = lines(), moved = order.join('\n') !== drag.before
    drag = null
    S.sorting = false
    S.sortedAt = Date.now()
    if (moved) {
      reorderAgents(list, order)
      guarded(async () => {
        try { await api('PUT', `/v1/agent-order/${list}`, { lines: order }) } finally { refreshAgents().catch(() => {}) }
      })
    }
    renderSidebar()
  }
  box.addEventListener('pointerdown', (e) => {
    // A team's own list is a sortable inside this one: what is held there
    // moves there, not the team.
    if (e.button !== 0 || box.children.length < 2 || e.target.closest('.sortable') !== box) return
    let item = e.target
    while (item && item.parentElement !== box) item = item.parentElement
    if (!item?.dataset.line) return
    const { clientX: x, clientY: y, pointerId: id } = e
    cancel()
    hold = { x, y, timer: setTimeout(() => start(item, id, y), 450) }
    S.sorting = true
  })
  box.addEventListener('pointermove', (e) => {
    if (hold && Math.hypot(e.clientX - hold.x, e.clientY - hold.y) > 8) cancel()
    if (!drag || e.pointerId !== drag.id) return
    e.preventDefault()
    drag.y = e.clientY
    place(e.clientY)
  })
  box.addEventListener('pointerup', end)
  box.addEventListener('pointercancel', end)
  box.addEventListener('pointerleave', () => { if (hold) cancel() })
  // A row being moved is not scrolled with, nor opened when it is let go,
  // nor given the long press's menu.
  box.addEventListener('touchmove', (e) => { if (drag) e.preventDefault() }, { passive: false })
  box.addEventListener('click', (e) => {
    if (Date.now() - (S.sortedAt || 0) < 400) { e.preventDefault(); e.stopPropagation() }
  }, true)
  box.addEventListener('contextmenu', (e) => { if (hold || drag) e.preventDefault() })
  return box
}

// ---- Dividers: thin lines between the live agents, kept in the daemon's
// order of the list as "divider:<id>", so every UI draws the same ones.

const isDivider = (line) => line.startsWith('divider:')

// liveRows are the live agents' rows with the dividers and the teams where
// the order has them; agents it has not placed come after, as the daemon
// sorts them, and so do teams, an empty one at the bottom. A team's agents
// are drawn in it, in the daemon's order.
function liveRows(live, row) {
  const teams = new Map((S.order?.teams || []).map((t) => [t.id, t]))
  const inTeam = (a) => !!a.team_id && teams.has(a.team_id)
  const members = new Map()
  for (const a of live.filter(inTeam)) {
    if (!members.has(a.team_id)) members.set(a.team_id, [])
    members.get(a.team_id).push(a)
  }
  const byLine = new Map(live.filter((a) => !inTeam(a)).map((a) => [a.line || a.id, a]))
  const out = [], drawn = new Set()
  const team = (id) => {
    if (drawn.has(id)) return
    drawn.add(id)
    out.push(teamGroup(teams.get(id), members.get(id) || [], row))
  }
  for (const l of S.order?.live || []) {
    if (isDivider(l)) out.push(dividerRow(l))
    else if (isTeam(l)) { if (teams.has(teamOf(l))) team(teamOf(l)) }
    else if (byLine.has(l) && !drawn.has(l)) { out.push(row(byLine.get(l))); drawn.add(l) }
  }
  for (const a of live) {
    if (inTeam(a)) team(a.team_id)
    else if (!drawn.has(a.line || a.id)) { out.push(row(a)); drawn.add(a.line || a.id) }
  }
  for (const id of teams.keys()) team(id)
  return out
}

function dividerRow(line) {
  return h('div', { class: 'divider', 'data-line': line, role: 'separator' },
    h('button', {
      class: 'divider-x', title: 'Remove this divider', 'aria-label': 'Remove divider',
      onclick: (e) => { e.stopPropagation(); setLiveOrder(liveLines().filter((l) => l !== line)) },
    }, icon('x')))
}

// ---- Teams: named groups of live agents, each one row of the live list
// ("team:<id>") that folds to its head, its agents moved within it alone.

const isTeam = (line) => line.startsWith('team:')
const teamOf = (line) => line.slice('team:'.length)

// teamRank is how much an agent's state wants a look, for a folded team's
// head to show its agents by: the most first.
const teamRank = { blocked: 0, error: 1, interrupted: 2, idle: 3, starting: 4, working: 4, parked: 5 }
const agentState = (a) => { const st = a.state?.status || 'idle'; return st === 'idle' && a.parked ? 'parked' : st }

function teamGroup(t, members, row) {
  const folded = S.foldedTeams.has(t.id)
  const toggle = () => {
    folded ? S.foldedTeams.delete(t.id) : S.foldedTeams.add(t.id)
    try { localStorage.setItem('psps.foldedTeams', JSON.stringify([...S.foldedTeams])) } catch {}
    renderSidebar()
  }
  // Folded, the head has a dot for each of its agents, those that want a
  // look first, so none is out of sight.
  // An agent whose turn ended unopened is ringed in red, and first of its state.
  const dots = folded ? members.map((a) => ({ st: agentState(a), u: S.unread.has(a.id) }))
    .sort((x, y) => (teamRank[x.st] ?? 9) - (teamRank[y.st] ?? 9) || y.u - x.u) : []
  const holdsOpen = folded && S.view === 'agent' && members.some((a) => a.id === S.sel)
  return h('div', { class: 'team' + (folded ? ' folded' : ''), 'data-line': 'team:' + t.id },
    h('div', { class: 'team-head' + (holdsOpen ? ' selected' : '') },
      h('button', { class: 'team-toggle', 'aria-expanded': String(!folded), title: folded ? 'Unfold' : 'Fold', onclick: toggle },
        h('span', { class: 'caret' }, icon(folded ? 'chevron-right' : 'chevron-down')),
        h('span', { class: 'name' }, t.name),
        h('span', { class: 'count' }, String(members.length)),
        dots.length ? h('span', { class: 'team-dots', 'aria-label': dots.map((d) => d.st + (d.u ? ' (not opened)' : '')).join(', ') },
          dots.slice(0, 8).map((d) => h('span', { class: 'dot ' + d.st + (d.u ? ' unread' : '') })),
          dots.length > 8 ? h('span', { class: 'more' }, `+${dots.length - 8}`) : null) : null),
      h('button', { class: 'team-edit', title: 'The team: its name, its lead and the terms', 'aria-label': `Edit team ${t.name}`, onclick: () => editTeam(t) },
        icon('pencil'))),
    t.run ? teamRunLine(t.run, () => editTeam(t)) : null,
    t.run ? teamEvalLine(t.run, () => editTeam(t)) : null,
    folded ? null : members.length
      ? sortable('team:' + t.id, members.map(row))
      : h('div', { class: 'team-empty' }, 'No agents; an agent joins in Edit.'))
}

// teamRunLine is what a team's head says of its lead's run: what it does,
// its spending (of its budget, if it has one), and its best score.
function teamRunLine(run, open) {
  const text = `${run.status} · ${tokens(run.spent)}` + (run.budget ? ` of ${tokens(run.budget)}` : '') + (run.best != null ? ` · best ${score2(run.best)}` : '')
  return h('button', { type: 'button', class: 'team-run' + (run.status.startsWith('stopped') ? ' stopped' : run.status.startsWith('paused') ? ' paused' : ''), title: text, onclick: open }, text)
}

// editTeam is a team's dialog: its name, its lead and the lead's terms,
// and how the lead's run goes; or the team deleted, its agents left where
// it was.
async function editTeam(t) {
  let run = null
  if (t.run) run = await api('GET', `/v1/teams/${enc(t.id)}/run`).catch(() => null)
  const name = h('input', { class: 'dialog-input', value: t.name, autocomplete: 'off' })
  // A lead's own agents lead nothing.
  const members = S.agents.filter((a) => !a.archived_at && a.team_id === t.id && !a.made_by)
  const leading = run && members.find((a) => (a.line || a.id) === run.lead_line)
  // A lead with no live session stays the lead until it is changed.
  const gone = run && !leading ? [{ value: run.lead, label: `${run.lead} (not live)` }] : []
  const lead = dropdown([{ value: '', label: 'No lead' }, ...gone,
    ...members.map((a) => ({ value: a.id, label: `${shownID(a)} · ${agentTitle(a)} · ${a.isolation}` }))],
  { value: leading?.id || gone[0]?.value || '', label: 'Lead' })
  const st = run?.settings || {}
  const input = (value, placeholder, mode) => h('input', { class: 'dialog-input', value: value ?? '', placeholder, autocomplete: 'off', inputmode: mode || null })
  const budget = input(st.budget ? budgetText(st.budget) : '', 'none; or 5M, 800k')
  const maxLive = input(st.max_live, '3', 'numeric')
  const turn = input(st.turn_minutes, '60', 'numeric')
  const ceiling = input(st.plan_ceiling, '90', 'numeric')
  const evalCmd = input(st.eval, 'none')
  const evalDir = input(st.eval_dir, "the lead's directory")
  S.isolations ||= await api('GET', '/v1/isolations').catch(() => [])
  // An eval isolation is another than the lead's, so the choice follows
  // the lead chosen.
  const evalIsos = () => {
    const own = S.agents.find((a) => a.id === lead.value)?.isolation
    return [{ value: '', label: "The lead's own" }, ...S.isolations.map((i) => i.name).filter((n) => n !== own)]
  }
  const evalIso = dropdown(evalIsos(), { value: run?.eval_isolation || '', label: 'Eval isolation' })
  lead.addEventListener('change', () => evalIso.setOptions(evalIsos()))
  const evalMin = input(st.eval_minutes, '20', 'numeric')
  const target = input(st.target, 'none', 'decimal')
  const patience = input(st.patience, '3', 'numeric')
  const field = (label, el, hint) => h('label', { class: 'dialog-field' }, label, el, hint ? h('span', { class: 'field-hint' }, hint) : null)
  // Without a budget, what holds the run back, and what ends it.
  const noBudget = h('p', { class: 'field-hint' })
  const warn = () => {
    noBudget.hidden = parseTokens(budget.value) !== 0
    noBudget.textContent = "No budget: only the plan ceiling holds the run back, and it counts the account's whole use, yours too; lower it to keep some of the week." +
      (evalCmd.value.trim() ? '' : ' With no eval either, psps never ends the run or wakes the lead: the team spends while the lead keeps work going, and stands idle once it stops.')
  }
  budget.addEventListener('input', warn)
  evalCmd.addEventListener('input', warn)
  warn()
  const compactBox = h('input', { type: 'checkbox', checked: !!st.compact_first })
  const compactFirst = h('label', { class: 'check lead-teams', title: "psps sends the lead /compact once the round's eval has ended, and the round's news and score after it" },
    compactBox, h('span', null, "Compact before a round's result: the lead's conversation is compacted (/compact) before it hears the round's end and score"))
  // The approver: any live agent no lead made; the lead itself is refused
  // on saving. Only the operator sets it.
  const approverNow = st.approver ? S.agents.find((a) => !a.archived_at && (a.line || a.id) === st.approver) : null
  const approverGone = st.approver && !approverNow ? [{ value: st.approver, label: `${run.approver || st.approver} (not live)` }] : []
  const approver = dropdown([{ value: '', label: 'None' }, ...approverGone,
    ...S.agents.filter((a) => !a.archived_at && !a.made_by).map((a) => ({ value: a.id, label: `${shownID(a)} · ${agentTitle(a)} · ${a.isolation}` }))],
  { value: approverNow?.id || approverGone[0]?.value || '', label: 'Approver' })
  // The mode matters only with an approver, so it shows only then, with
  // what the mode chosen does.
  let approveMode = st.approve_mode || 'gate'
  const modes = [['gate', 'Gate', 'The eval waits for its verdict. An approval runs it; a rejection sends the round back to the lead, using no patience. No deadline.'],
    ['notify', 'Notify', 'The eval runs as usual; the approver is only told of the round and its score.']]
  const modeSeg = h('div', { class: 'segmented', role: 'radiogroup', 'aria-label': 'Mode' })
  const modeHint = h('span', { class: 'field-hint' })
  const approverHint = h('span', { class: 'field-hint' }, 'Not the lead nor one of its own. It answers on its team service (teams on in its isolation), or you decide here.')
  const modeField = h('div', { class: 'dialog-field' }, h('span', null, 'Mode'), modeSeg, modeHint)
  const drawMode = () => {
    modeSeg.replaceChildren(...modes.map(([v, label]) => h('button', {
      type: 'button', role: 'radio', 'aria-checked': String(v === approveMode), class: v === approveMode ? 'on' : '',
      onclick: () => { approveMode = v; drawMode(); modeSeg.querySelector('.on')?.focus() },
    }, label)))
    modeHint.textContent = modes.find(([v]) => v === approveMode)[2]
    approverHint.hidden = modeField.hidden = !approver.value
  }
  approver.addEventListener('change', drawMode)
  drawMode()
  const approval = h('section', { class: 'lead-approval' },
    h('h4', null, 'Round approval'),
    h('p', { class: 'field-hint' }, 'Each round goes to an approver before its eval runs.'),
    h('label', { class: 'dialog-field' }, 'Approver', approver, approverHint),
    modeField)
  const turnOn = h('input', { type: 'checkbox', checked: true })
  const offIn = h('span')
  const teamsOff = h('label', { class: 'check lead-teams', hidden: true }, turnOn, h('span', null, 'Turn teams on in ', offIn,
    ", the lead's isolation: only there does it act. Its agents' processes are replaced when next idle; their sessions are kept."))
  const terms = h('div', { class: 'lead-terms' },
    h('div', { class: 'lead-grid' },
      field('Budget (tokens)', budget), field('Live at most', maxLive), field('Turn limit (min)', turn), field('Plan ceiling (%)', ceiling)),
    noBudget,
    field('Eval', evalCmd, 'A shell command; its last "SCORE: x" line is the score, higher better. In the lead\'s isolation it runs as the agents, who can change it and what it reads.' +
      (st.eval_set_by ? ` Agent ${st.eval_set_by} set it, through control; saved changed, it is yours.` : '')),
    field('Eval isolation', evalIso, 'Another isolation, with no services but control, scores a copy of the eval directory made for each eval, as a user who can change nothing else there: out of the agents\' reach. Keep the grader and its data there, and take only what is scored from the copy; ~ in the eval is then an empty home, so give the grader\'s path in full. The lead hears only the score. An agent of yours there runs as the grader\'s owner: keep it from what the eval leaves.'),
    h('div', { class: 'lead-grid' },
      field('Eval directory', evalDir), field('Eval limit (min)', evalMin), field('Target', target, 'ends the run once reached'), field('Patience (evals)', patience, 'without a new best, end the run')),
    compactFirst,
    approval,
    teamsOff)
  let off = null
  const shown = async () => {
    const a = members.find((m) => m.id === lead.value)
    terms.hidden = !lead.value
    teamsOff.hidden = true
    off = null
    if (!a) return
    const xs = await api('GET', `/v1/isolations/${enc(a.isolation)}/services`).catch(() => null)
    if (lead.value !== a.id || !xs) return
    if (!xs.some((x) => x.service === 'team' && x.enabled)) {
      off = a.isolation
      offIn.textContent = a.isolation
      teamsOff.hidden = false
      // Ticked for a lead chosen now: saving the lead it has turns nothing on.
      turnOn.checked = a.id !== leading?.id
    }
  }
  lead.addEventListener('change', shown)
  shown()
  // terms are the form as the daemon takes them; a term left empty keeps
  // what it was, or its default.
  const asked = () => {
    const req = { agent: lead.value }
    const n = parseTokens(budget.value)
    if (n == null) throw new Error(`The budget "${budget.value.trim()}" is not a count of tokens (5M, 800k, 2000000), or none.`)
    req.budget = n
    for (const [k, el, what] of [['max_live', maxLive, 'Live at most'], ['turn_minutes', turn, 'The turn limit'], ['plan_ceiling', ceiling, 'The plan ceiling'],
      ['eval_minutes', evalMin, 'The eval limit'], ['patience', patience, 'The patience']]) {
      const v = el.value.trim()
      if (!v) continue
      if (!/^\d+$/.test(v)) throw new Error(`${what} is a whole number, not "${v}".`)
      req[k] = Number(v)
    }
    req.eval = evalCmd.value.trim()
    if (evalDir.value.trim()) req.eval_dir = evalDir.value.trim()
    req.eval_isolation = evalIso.value
    const tv = target.value.trim()
    if (!tv) req.no_target = true
    else if (Number.isFinite(Number(tv))) req.target = Number(tv)
    else throw new Error(`The target is a number, not "${tv}".`)
    req.compact_first = compactBox.checked
    req.approver = approver.value
    req.approve_mode = approveMode
    return req
  }
  const before = run ? JSON.stringify(asked()) : null
  // A round waiting for the approver: the operator may decide in its place.
  const decide = (verdict) => async (e) => {
    e.target.closest('dialog').close('cancel')
    const note = h('textarea', { class: 'dialog-input', rows: 4, placeholder: verdict === 'reject' ? 'What is wrong with the round; the lead hears it' : 'Optional; the lead hears it with the score' })
    await ask({
      title: `${verdict === 'reject' ? 'Reject' : 'Approve'} review #${run.review.id}?`,
      body: verdict === 'reject' ? `No eval runs and no patience is used; the lead hears your note. The round goes to ${run.approver} again once a turn of the lead's agents ends after this.`
        : 'The eval runs now, and the lead hears your note with its score.',
      fields: [h('label', { class: 'dialog-field' }, 'Note', note)],
      ok: verdict === 'reject' ? 'Reject' : 'Approve', danger: verdict === 'reject',
      onOk: async () => {
        if (verdict === 'reject' && !note.value.trim()) throw new Error('Say why the round is rejected: the lead hears it.')
        await api('POST', `/v1/teams/${enc(t.id)}/review`, { verdict, note: note.value.trim() })
        toast(`Team "${t.name}": review #${run.review.id} ${verdict === 'reject' ? 'rejected' : 'approved'}`)
      },
    })
    await guarded(refreshAgents)
  }
  const reviewView = run?.review && h('div', { class: 'lead-review' },
    h('div', null, h('strong', null, `Review #${run.review.id}: `),
      `the round waits for ${run.approver}${run.approver_live ? '' : ' (no live session)'} to approve its eval, since ${agoShort(run.review.asked_at)}, as ${run.review.why}.`),
    h('div', { class: 'lead-actions' },
      h('button', { type: 'button', class: 'btn small primary', onclick: decide('approve') }, 'Approve'),
      h('button', { type: 'button', class: 'btn small danger', onclick: decide('reject') }, 'Reject')))
  const runView = run && h('div', { class: 'lead-run' + (run.stopped ? ' stopped' : '') },
    h('div', null, h('strong', null, `${run.lead} leads it: `), run.status),
    reviewView || null,
    h('div', null, `Spent ${tokens(run.spent.input + run.spent.output)}` + (run.settings.budget ? ` of ${tokens(run.settings.budget)} tokens` : ' tokens, no budget') +
      (run.best ? ` · best ${score2(run.best.score)} at eval #${run.best.id}, ${run.stale} since without a new one` : '')),
    h('div', { class: 'lead-actions' },
      run.settings.eval || run.settings.eval_job ? h('button', {
        type: 'button', class: 'btn small', disabled: run.evaluating, title: run.evaluating ? 'An eval is under way' : null, onclick: async (e) => {
          e.target.disabled = true
          await guarded(async () => { await api('POST', `/v1/teams/${enc(t.id)}/eval`); toast('The eval runs; its result goes to the lead') })
        },
      }, 'Run eval now') : null,
      run.best ? h('button', {
        type: 'button', class: 'btn small', title: 'The evals so far count for no best, the target or the patience; the next scored eval is the best',
        onclick: async (e) => {
          e.target.closest('dialog').close('cancel')
          if (!await ask({ title: 'Reset the best score?', body: `The best is ${run.best.score}, at eval #${run.best.id}. The evals so far will count no more for the best, the target or the patience: the next scored eval is the best. The lead is told.`, ok: 'Reset' })) return
          await guarded(async () => {
            await api('DELETE', `/v1/teams/${enc(t.id)}/best`)
            toast(`Team "${t.name}": the best is reset`)
            await refreshAgents()
          })
        },
      }, 'Reset best') : null,
      run.stopped ? h('button', {
        type: 'button', class: 'btn small primary', onclick: async (e) => {
          e.target.closest('dialog').close('cancel')
          await guarded(async () => {
            await api('PUT', `/v1/teams/${enc(t.id)}/lead`, { agent: run.lead })
            toast(`Team "${t.name}" runs again`)
            await refreshAgents()
          })
        },
      }, 'Resume') : null))
  const del = h('button', {
    type: 'button', class: 'btn danger', onclick: async (e) => {
      e.target.closest('dialog').close('cancel')
      if (!await ask({ title: `Delete team "${t.name}"?`, body: 'Its agents stay where it was in the sidebar, in no team. Nothing else about them changes.', ok: 'Delete', danger: true })) return
      await guarded(async () => {
        await api('DELETE', `/v1/teams/${enc(t.id)}`)
        S.foldedTeams.delete(t.id)
        toast(`Team "${t.name}" deleted`)
        await refreshAgents()
      })
    },
  }, icon('trash-2'), ' Delete team')
  const ok = await ask({
    title: 'Team',
    body: 'A team groups agents in the sidebar; they move within it. Its lead makes agents of its own in the team and hears when their turns end, within the terms below.',
    wide: true,
    fields: [h('label', { class: 'dialog-field' }, 'Name', name),
      runView || null,
      run ? evalSection(t, run) : null,
      h('div', { class: 'dialog-field' }, 'Lead', lead),
      terms,
      run?.stopped ? h('p', { class: 'field-hint' }, 'Saving a change to the lead or its terms resumes the stopped run.') : null,
      h('div', { class: 'team-delete' }, del)].filter(Boolean),
    ok: 'Save',
    onOk: async () => {
      const n = name.value.trim()
      if (!n) throw new Error('A name is required.')
      const req = lead.value ? asked() : null
      if (n !== t.name) await api('PATCH', `/v1/teams/${enc(t.id)}`, { name: n })
      if (!req) {
        if (run) await api('DELETE', `/v1/teams/${enc(t.id)}/lead`)
        return
      }
      if (JSON.stringify(req) !== before) await api('PUT', `/v1/teams/${enc(t.id)}/lead`, req)
      if (off && turnOn.checked) await api('PUT', `/v1/isolations/${enc(off)}/services/team`, {})
    },
  })
  if (ok) await guarded(refreshAgents)
}

// ---- A team's eval, as it is now: under way (queued on its runner, or
// running against how long the last few took), waiting on the round, due
// (the round ended and none started: a minute of that is something
// wrong), or the last one's result.

const score2 = (v) => String(Math.round(v * 100) / 100)
const nsToMs = (ns) => (ns || 0) / 1e6
function evClock(ms) {
  const s = Math.max(0, Math.round(ms / 1000))
  const m = Math.floor(s / 60)
  if (m >= 60) return `${Math.floor(m / 60)}h ${String(m % 60).padStart(2, '0')}m`
  return m ? `${m}m ${String(s % 60).padStart(2, '0')}s` : `${s}s`
}
function agoShort(t) {
  const s = Math.max(0, (Date.now() - new Date(t).getTime()) / 1000)
  if (s < 60) return 'just now'
  if (s < 3600) return `${Math.round(s / 60)}m ago`
  if (s < 86400) return `${Math.round(s / 3600)}h ago`
  return `${Math.round(s / 86400)}d ago`
}
const nth = (n) => n + ((n % 100 >= 11 && n % 100 <= 13) ? 'th' : ['th', 'st', 'nd', 'rd'][n % 10] || 'th')
const EVAL_FRESH = 10 * 60 * 1000
const DUE_AFTER = 60 * 1000

// evalState is a run's eval for the head and the dialog alike: its words,
// its tone (quiet, live, ok, warn, err), its mark and, under way, how far.
function evalState(run) {
  const ev = run.eval
  if (!ev || ev.state === 'none') return null
  const last = run.last
  const lastText = last ? ` · last #${last.id} ${last.score != null ? score2(last.score) : last.not_run ? 'not run' : 'no score'}, ${agoShort(last.at)}` : ''
  const usual = nsToMs(ev.usual)
  const on = ev.runner ? ` on ${ev.runner}` : ev.isolation ? ` in ${ev.isolation}` : ''
  if (ev.state === 'running') {
    const el = Date.now() - new Date(ev.since).getTime()
    const slow = usual && el > 2 * usual
    return {
      tone: slow ? 'warn' : 'live', mark: 'pulse', elapsed: el, usual,
      pct: usual ? Math.min(100, (el / usual) * 100) : null,
      text: slow ? `Eval #${ev.number} running · ${evClock(el)}, usually ~${evClock(usual)}`
        : `Eval #${ev.number} running${on} · ${evClock(el)}` + (usual ? ` of ~${evClock(usual)}` : ''),
    }
  }
  if (ev.state === 'queued') {
    return { tone: 'live', mark: 'ring', text: `Eval #${ev.number} queued${on}` + (ev.place ? ` · ${nth(ev.place)} in line` : '') }
  }
  if (ev.state === 'approval') {
    const el = Date.now() - new Date(ev.since).getTime()
    return { tone: 'live', mark: 'ring', text: `Round waits for ${ev.waiting}'s approval · ${evClock(el)}` }
  }
  if (ev.state === 'due') {
    const el = Date.now() - new Date(ev.since).getTime()
    return el < DUE_AFTER
      ? { tone: 'live', mark: 'ring', text: `Round ended · eval #${ev.number} starting` }
      : { tone: 'warn', mark: 'dot', due: true, text: `Round ended ${evClock(el)} ago, eval not started` }
  }
  // Nothing under way: a result just told stands out for a while.
  if (last && Date.now() - (new Date(last.at).getTime() + nsToMs(last.took)) < EVAL_FRESH) {
    const ended = new Date(new Date(last.at).getTime() + nsToMs(last.took))
    if (last.score == null) return { tone: 'err', mark: 'square', text: `#${last.id} ${last.not_run ? 'not run' : 'no score'}` + (last.note ? `: ${last.note}` : '') }
    return { tone: last.new_best ? 'ok' : 'quiet', mark: 'dot', text: `#${last.id} ${score2(last.score)}` + (last.new_best ? ' · new best' : '') + ` · ${agoShort(ended)}` }
  }
  if (ev.state === 'waiting') return { tone: 'quiet', mark: 'ring', text: 'Eval after this round' + lastText }
  return { tone: 'quiet', mark: 'ring', text: (last ? 'No change since the last eval' : 'No eval yet') + lastText }
}

function teamEvalLine(run, open) {
  const st = evalState(run)
  if (!st) return null
  return [
    h('button', { type: 'button', class: `team-eval ${st.tone}`, title: st.text, onclick: open },
      h('span', { class: `ev-mark ${st.mark}`, 'aria-hidden': 'true' }), h('span', { class: 'ev-text' }, st.text)),
    st.elapsed != null ? h('div', { class: `ev-bar ${st.tone}` + (st.pct == null ? ' unknown' : '') }, h('span', { style: `width: ${st.pct ?? 100}%` })) : null,
  ]
}

// evalSection is the eval in a team's dialog: the one under way, with
// Stop eval for a runner's job, and the last five.
function evalSection(t, run) {
  if (!run.settings.eval && !run.settings.eval_job) return null
  const ev = run.eval || {}
  const st = evalState({ eval: ev, last: run.evals.length ? { ...run.evals[run.evals.length - 1], new_best: run.best?.id === run.evals[run.evals.length - 1].id } : null })
  const s = run.settings
  const what = s.eval_job ? `a job of runner ${s.eval_job}` : run.eval_isolation ? `in isolation ${run.eval_isolation}` : "in the lead's isolation"
  const under = ev.state === 'running' || ev.state === 'queued'
  const stop = h('button', {
    type: 'button', class: 'btn small danger', onclick: async (e) => {
      e.target.closest('dialog').close('cancel')
      if (!await ask({ title: `Stop eval #${ev.number}?`, body: `Its job ${ev.job} is cancelled. The eval counts as not run, for no patience, and the lead hears so.`, ok: 'Stop eval', danger: true })) return
      await guarded(async () => {
        await api('DELETE', `/v1/teams/${enc(t.id)}/eval`)
        toast(`Team "${t.name}": eval #${ev.number} stopped`)
        await refreshAgents()
      })
    },
  }, 'Stop eval')
  const card = under || ev.state === 'due'
    ? h('div', { class: 'eval-now ' + (st?.tone || 'live') },
      h('div', { class: 'eval-now-head' },
        h('span', { class: `ev-mark ${st?.mark || 'ring'}`, 'aria-hidden': 'true' }),
        h('strong', null, ev.state === 'due' ? 'Round ended, no eval' : `#${ev.number} ${ev.state}`),
        ev.why ? h('span', { class: 'note' }, `as ${ev.why}`) : null,
        st?.elapsed != null ? h('span', { class: 'eval-clock' }, evClock(st.elapsed)) : null),
      st?.elapsed != null ? h('div', { class: `ev-bar big ${st.tone}` + (st.pct == null ? ' unknown' : '') }, h('span', { style: `width: ${st.pct ?? 100}%` })) : null,
      h('div', { class: 'eval-now-meta' },
        h('span', null, ev.state === 'due' ? `the round ended ${evClock(Date.now() - new Date(ev.since).getTime())} ago` : ev.started ? `started ${new Date(ev.started).toLocaleTimeString()}` : ev.place ? `${nth(ev.place)} in line on ${ev.runner}` : 'waiting for its runner'),
        h('span', null, [st?.usual ? `usually ~${evClock(st.usual)} (the last 5)` : null, ev.limit ? `limit ${evClock(nsToMs(ev.limit))}` : null].filter(Boolean).join(' · '))),
      ev.note ? h('div', { class: 'eval-now-meta' }, h('span', null, ev.note)) : null,
      h('div', { class: 'eval-now-actions' },
        ev.stoppable ? stop : null,
        ev.job ? h('span', { class: 'eval-job' }, `job ${ev.job} · psps job show ${ev.job}`) : null))
    : st ? h('p', { class: 'note' }, st.text) : null
  const evals = run.evals.slice(-5).reverse()
  const scores = run.evals.slice(-5).map((e) => e.score).filter((v) => v != null)
  const spark = scores.length > 1 ? sparkline(scores) : null
  return h('div', { class: 'eval-section' },
    h('div', { class: 'eval-section-head' }, h('strong', null, 'Eval'), h('span', { class: 'note' }, `${what} · patience ${s.patience}`)),
    card,
    evals.length ? h('div', { class: 'eval-recent' },
      h('div', { class: 'eval-recent-head' }, h('span', { class: 'label' }, 'Recent'), spark),
      h('div', { class: 'eval-table' },
        h('div', { class: 'eval-row head' }, h('span', null, '#'), h('span', null, 'when'), h('span', null, 'why'), h('span', null, 'took'), h('span', null, 'score')),
        evals.map((e) => h('div', { class: 'eval-row', title: e.output ? e.output.slice(-600) : '' },
          h('span', { class: 'muted' }, String(e.id)),
          h('span', null, new Date(e.at).toLocaleString(undefined, { month: 'short', day: 'numeric', hour: '2-digit', minute: '2-digit' })),
          h('span', { class: 'muted ellipsis' }, e.why),
          h('span', { class: 'muted' }, evClock(nsToMs(e.took))),
          h('span', { class: 'score' + (run.best?.id === e.id ? ' best' : '') }, e.score != null ? score2(e.score) + (run.best?.id === e.id ? ' ★' : '') : e.not_run ? 'not run' : 'no score'))))) : null)
}

// sparkline is a few scores as a line, the last one marked.
function sparkline(vs) {
  const w = 120, hgt = 28, lo = Math.min(...vs), hi = Math.max(...vs)
  const x = (i) => 4 + (i * (w - 8)) / (vs.length - 1)
  const y = (v) => hi === lo ? hgt / 2 : 3 + (1 - (v - lo) / (hi - lo)) * (hgt - 6)
  const svg = document.createElementNS(svgNS, 'svg')
  for (const [k, v] of Object.entries({ width: w, height: hgt, viewBox: `0 0 ${w} ${hgt}`, role: 'img', 'aria-label': 'The last scores: ' + vs.map(score2).join(', '), class: 'spark' })) svg.setAttribute(k, v)
  const pl = document.createElementNS(svgNS, 'polyline')
  pl.setAttribute('points', vs.map((v, i) => `${x(i)},${y(v)}`).join(' '))
  const dot = document.createElementNS(svgNS, 'circle')
  for (const [k, v] of Object.entries({ cx: x(vs.length - 1), cy: y(vs[vs.length - 1]), r: 2.5 })) dot.setAttribute(k, v)
  svg.append(pl, dot)
  return svg
}

// parseTokens reads a budget: 5000000, 5M, 800k, or 0 for nothing or
// none; null if it is none of these.
function parseTokens(s) {
  s = s.trim()
  if (!s || /^none$/i.test(s)) return 0
  const m = /^(\d+(?:\.\d+)?)\s*([km]?)$/i.exec(s)
  if (!m) return null
  return Math.round(Number(m[1]) * ({ k: 1e3, m: 1e6 }[m[2].toLowerCase()] || 1))
}

// budgetText is a budget as it is typed back: 5M, 800k, or the number.
function budgetText(n) {
  return n % 1e6 === 0 ? n / 1e6 + 'M' : n % 1e3 === 0 ? n / 1e3 + 'k' : String(n)
}

// teamInput is a text field for a team's name that offers the teams
// there are; a name no team has makes one.
function teamInput(value) {
  const id = 'teams-' + ++teamInput.n
  return [
    h('input', { class: 'dialog-input', value: value || '', list: id, autocomplete: 'off', placeholder: 'none' }),
    h('datalist', { id }, (S.order?.teams || []).map((t) => h('option', { value: t.name }))),
  ]
}
teamInput.n = 0

// liveLines are the live list as it is drawn, from the top.
function liveLines() {
  const box = document.querySelector('#sidebar .agent-list > .sortable')
  return box ? [...box.children].map((c) => c.dataset.line) : []
}

function addDivider() {
  const id = [...crypto.getRandomValues(new Uint8Array(4))].map((b) => b.toString(16).padStart(2, '0')).join('')
  setLiveOrder([...liveLines(), 'divider:' + id])
}

function setLiveOrder(lines) {
  S.order = { ...S.order, live: lines }
  renderSidebar()
  guarded(async () => {
    try { await api('PUT', '/v1/agent-order/live', { lines }) } finally { refreshAgents().catch(() => {}) }
  })
}

// reorderAgents puts one sidebar list's agents in the order of its lines at
// once, in the places they had among all agents, before the daemon answers.
function reorderAgents(list, order) {
  const inList = isTeam(list) ? (a) => !a.archived_at && a.team_id === teamOf(list) : (a) => !a.archived_at
  const rank = new Map(order.map((l, i) => [l, i]))
  const mine = S.agents.filter(inList)
    .map((a, i) => [rank.get(a.line || a.id) ?? order.length + i, a])
    .sort((x, y) => x[0] - y[0]).map(([, a]) => a)
  let k = 0
  S.agents = S.agents.map((a) => inList(a) ? mine[k++] : a)
  S.order = { ...S.order, [list]: order }
}

// archivedEntries are the archived agents by line, in order, each line
// where its first session is.
function archivedEntries(archived) {
  const out = [], byLine = new Map()
  for (const a of archived) {
    const line = a.line || a.id
    let e = byLine.get(line)
    if (!e) { e = { line, sessions: [] }; byLine.set(line, e); out.push(e) }
    e.sessions.push(a)
  }
  return out
}

function agentView() {
  const a = agentById(S.sel)
  S.drawn = null
  if (!a) {
    const frag = document.createDocumentFragment()
    frag.append(topbar('psps'), h('div', { class: 'page' }, h('div', { class: 'empty' },
      h('p', null, S.agents.length ? 'No agent selected.' : 'No agents yet.'),
      h('button', { class: 'btn primary', onclick: () => show('new') }, 'Start an agent'))))
    return frag
  }
  S.drawn = a.id
  const frag = document.createDocumentFragment()
  frag.append(
    h('div', { id: 'topbar-slot' }),
    h('div', { class: 'convo', id: 'convo', onscroll: (e) => onConvoScroll(e.target, a.id) },
      h('div', { class: 'last-sent-bar', id: 'last-sent', hidden: true }, h('div', { class: 'last-sent-strip' }, h('div', { class: 'last-sent-row' },
        h('button', { type: 'button', class: 'last-sent' }),
        h('button', { type: 'button', class: 'sent-nav', 'data-go': 'up', title: 'The message you sent before', 'aria-label': 'Previous message you sent', onclick: () => goSent(-1) }, icon('chevron-up')),
        h('button', { type: 'button', class: 'sent-nav', 'data-go': 'down', title: 'The message you sent after', 'aria-label': 'Next message you sent', onclick: () => goSent(1) }, icon('chevron-down')),
        h('button', { type: 'button', class: 'sent-nav', 'data-go': 'end', title: 'To the end', 'aria-label': 'Scroll to the end', onclick: () => { const b = document.getElementById('convo'); b.scrollTo({ top: b.scrollHeight, behavior: 'smooth' }) } }, icon('arrow-down-to-line'))))),
      h('div', { class: 'convo-inner', id: 'convo-inner' })),
    h('div', { class: 'ask-jump-slot' }, h('button', { type: 'button', class: 'ask-jump', id: 'ask-jump', hidden: true }, icon('chevron-down'), h('span', null))),
    composer(a),
  )
  queueMicrotask(() => { renderTopbar(); renderConvo(true) })
  return frag
}

// forkedFrom is the ID of the agent a was forked from, while psps has it.
function forkedFrom(a) {
  const from = a.forked_from && agentById(a.forked_from)
  return from ? shownID(from) : ''
}

function renderTopbar() {
  const slot = document.getElementById('topbar-slot')
  const a = agentById(S.sel)
  if (!slot || !a) return
  const st = a.archived_at ? 'archived' : a.state?.status || 'idle'
  const working = st === 'working'
  // replaceChildren turns null into the text "null"; keep only nodes.
  slot.replaceChildren(...[
    topbar(agentTitle(a), a.host ? `${a.kind} · on the Mac · ${tildeHome(a.workdir)}`
      : `${a.kind} · ${a.isolation} · ${shownID(a)}` + (a.team ? ` · team ${a.team}` : '') + (forkedFrom(a) ? ` · fork of ${forkedFrom(a)}` : ''),
      h('span', { class: 'chip ' + st }, st),
      a.host ? h('span', { class: 'chip host', title: 'It runs on this Mac, outside every isolation' }, icon('monitor'), 'host') : null,
      h('div', { class: 'actions' },
        working ? h('button', { class: 'btn stop', onclick: () => guarded(() => api('POST', `/v1/agents/${a.id}/interrupt`)) }, icon('square'), h('span', { class: 'label' }, ' Stop')) : null,
        h('button', { class: 'btn ghost', title: 'Its mail: what it was sent and what it wrote', onclick: () => openMail(a) },
          icon('mail'), h('span', { class: 'label' }, ' Mail'), a.inbox ? h('span', { class: 'badge' }, String(a.inbox)) : null),
        (a.watching || a.watched_by?.length) ? h('button', { class: 'btn ghost' + (a.watch_stalled ? ' warn' : ''), title: watchTitle(a), onclick: () => openWatches(a) },
          icon('target'), h('span', { class: 'label' }, a.watching ? ' Watching' : ' Awaited'), a.watching ? h('span', { class: 'badge' }, String(a.watching)) : null) : null,
        a.archived_at ? null : h('button', { class: 'btn ghost', title: 'Cron: messages sent to it on a schedule', onclick: () => openCrons(a) },
          icon('clock'), h('span', { class: 'label' }, ' Cron'), a.crons ? h('span', { class: 'badge' }, String(a.crons)) : null),
        a.archived_at ? null : h('button', { class: 'btn ghost', title: 'TODO: prompts put aside to send it later', onclick: () => openTodos(a) },
          icon('list-todo'), h('span', { class: 'label' }, ' TODO'), a.todos ? h('span', { class: 'badge' }, String(a.todos)) : null),
        h('button', { class: 'btn ghost', title: 'Name, ID and team', onclick: () => editAgent(a) }, icon('pencil'), h('span', { class: 'label' }, ' Edit')),
        a.host ? null : h('button', {
          class: 'btn ghost', title: 'Fork: a new agent in its directory that knows its conversation so far', onclick: async () => {
            if (!await ask({
              title: `Fork "${agentTitle(a)}"?`,
              body: `A new agent starts in ${a.workdir}, in ${a.isolation}, knowing this conversation so far. It has no crons or mail, and both work in the one directory.`,
              ok: 'Fork',
            })) return
            await guarded(async () => {
              const f = await api('POST', `/v1/agents/${enc(a.id)}/fork`)
              toast(`Forked as ${shownID(f)}`)
              await refreshAgents()
              select(f.id)
            })
          },
        }, icon('git-fork'), h('span', { class: 'label' }, ' Fork')),
        a.archived_at ? h('button', {
          class: 'btn ghost', title: 'Restore: carry on where it left off', onclick: async () => {
            const live = S.agents.find((o) => !o.archived_at && o.line === a.line)
            if (!await ask({
              title: `Restore "${agentTitle(a)}"?`,
              body: live
                ? `It carries on where it left off, as ${shownID(live)}. "${agentTitle(live)}", the session there now, is archived, and its crons and waiting mail move over.`
                : 'It carries on where it left off.',
              ok: 'Restore',
            })) return
            await guarded(async () => {
              const back = await api('POST', `/v1/agents/${enc(a.id)}/restore`)
              toast(`Restored as ${shownID(back)}`)
              await refreshAgents()
              render()
            })
          },
        }, icon('archive-restore'), h('span', { class: 'label' }, ' Restore')) : null,
        a.archived_at ? h('button', {
          class: 'btn ghost danger', title: 'Delete', onclick: async () => {
            const purge = h('input', { type: 'checkbox' })
            if (!await ask({
              title: `Delete "${agentTitle(a)}"?`,
              body: `Its history and its log in ${a.isolation} are removed. This cannot be undone.`,
              extra: h('label', { class: 'check' }, purge, `Also remove its workspace and session transcript from ${a.isolation}`),
              ok: 'Delete', danger: true,
            })) return
            await guarded(async () => {
              await api('DELETE', `/v1/agents/${enc(a.id)}` + (purge.checked ? '?purge=1' : ''))
              toast(purge.checked ? `Deleted, with its workspace` : `Deleted; its workspace stays in ${a.isolation}`)
              S.sel = null
              await refreshAgents()
              render()
            })
          },
        }, icon('trash-2'), h('span', { class: 'label' }, ' Delete')) : null,
        a.archived_at ? null : h('button', { class: 'btn ghost', title: 'Archive (x)', onclick: () => askArchive(a) },
          icon('archive'), h('span', { class: 'label' }, ' Archive')))),
    a.host && !a.archived_at ? h('div', { class: 'banner host-banner' }, icon('triangle-alert'),
      ' Runs on this Mac as you, outside every isolation: it can read and change whatever you can.') : null,
    a.problem ? h('div', { class: 'banner' }, a.problem) : null,
  ].filter(Boolean))
  fitTopbar()
}

// fitTopbar keeps the agent's name readable: when the actions crowd it,
// they lose their labels first, then the last of them go behind ⋯.
const titleMin = 140
function fitTopbar() {
  const bar = document.querySelector('#topbar-slot .topbar')
  const actions = bar?.querySelector('.actions')
  if (!actions) return
  let more = actions.querySelector('.more-btn'), menu = bar.querySelector('.more-menu')
  if (!more) {
    menu = h('div', { class: 'more-menu', hidden: true })
    more = h('button', {
      class: 'btn ghost more-btn', title: 'More', 'aria-label': 'More', hidden: true,
      onclick: (e) => { e.stopPropagation(); S.moreOpen = menu.hidden; menu.hidden = !menu.hidden },
    }, icon('ellipsis'))
    // A choice in it closes it, after the choice is taken.
    menu.addEventListener('click', () => { S.moreOpen = false; menu.hidden = true })
    actions.append(more)
    bar.append(menu)
  }
  actions.prepend(...menu.children)
  actions.append(more)
  bar.classList.remove('compact')
  more.hidden = true
  const crowded = () => bar.querySelector('.title').getBoundingClientRect().width < titleMin || bar.scrollWidth > bar.clientWidth
  if (crowded()) bar.classList.add('compact')
  if (crowded()) {
    more.hidden = false
    // Stop stays out: it is wanted at once.
    let b
    while (crowded() && (b = more.previousElementSibling) && actions.firstElementChild !== b) menu.prepend(b)
  }
  // A count behind ⋯ (mail, crons, TODO) still shows that there is one.
  more.classList.toggle('has-badge', !!menu.querySelector('.badge'))
  menu.hidden = !(S.moreOpen && menu.children.length)
}
window.addEventListener('resize', fitTopbar)
document.addEventListener('click', (e) => {
  const menu = document.querySelector('.more-menu')
  if (menu && !menu.hidden && !menu.contains(e.target)) { S.moreOpen = false; menu.hidden = true }
})

// watchTitle says who waits on whom, for the top bar's watch button.
function watchTitle(a) {
  const says = []
  if (a.watching) says.push(`It waits to hear what came of ${plural(a.watching, 'message')} it sent`)
  if (a.watched_by?.length) says.push(`${a.watched_by.join(', ')} ${a.watched_by.length === 1 ? 'waits' : 'wait'} to hear what comes of its work: what you have it do reaches them too`)
  if (a.watch_stalled) says.push('Stalled: an agent lost its work with its process, and goes on only once you resume it')
  return says.join('. ')
}

// openWatches shows who an agent waits on, and who waits on it: psps tells
// a sender what came of its message once its agent has finished with it.
// A watch can be dropped, which tells the sender nothing.
async function openWatches(a) {
  const box = h('div', { class: 'inbox' })
  const states = {
    waiting: (w) => ['idle', 'not read yet'],
    reading: (w) => ['working', 'at work'],
    background: (w) => ['working', `${plural(w.background, 'task')} in the background`],
    stalled: (w) => ['interrupted', `stalled: ${plural(w.stalled, 'task')} lost, resume it`],
  }
  const draw = async () => {
    const v = await guarded(() => api('GET', `/v1/agents/${enc(a.id)}/watches`))
    if (!v) return
    const row = (w, sent) => {
      const [chip, label] = (states[w.state] || states.waiting)(w)
      const other = sent ? w.target : w.sender
      return h('div', { class: 'mail watch' + (w.state === 'stalled' ? ' stalled' : '') },
        h('div', { class: 'mail-head' },
          h('a', { href: `#/a/${other}`, title: 'Go to it', onclick: (e) => { e.preventDefault(); e.currentTarget.closest('dialog')?.close('cancel'); select(other) } },
            h('strong', null, sent ? `→ ${w.target_id || w.target}` : `← ${w.sender_id || w.sender}`)),
          h('span', { class: 'chip ' + chip }, label),
          h('button', {
            type: 'button', class: 'btn ghost danger', title: 'Stop waiting: the sender hears nothing more of it', onclick: () => guarded(async () => {
              await api('DELETE', `/v1/watches/${w.id}`)
              await draw()
            }),
          }, 'Drop')),
        h('div', { class: 'mail-meta' }, [`#${w.id}`, w.mode, `since ${new Date(w.created_at).toLocaleString()}`,
          w.last_activity ? `last activity ${new Date(w.last_activity).toLocaleString()}` : null].filter(Boolean).join(' · ')),
        h('div', { class: 'mail-text' }, w.excerpt || 'No message: from its next turn on'))
    }
    box.replaceChildren(...[
      v.sent.length ? h('h4', null, 'It waits on') : null, ...v.sent.map((w) => row(w, true)),
      v.received.length ? h('h4', null, 'Waiting on it') : null, ...v.received.map((w) => row(w, false)),
      !v.sent.length && !v.received.length ? h('p', { class: 'mail-lead' }, 'No one waits on anyone here now.') : null,
    ].filter(Boolean))
  }
  await draw()
  await ask({
    title: `Watches of "${agentTitle(a)}"`, cancel: 'Close',
    extra: h('div', null, h('p', { class: 'mail-lead' }, 'psps tells the sender what came of its message once the agent has finished with it, stopped, or waits for your approval. One stalled lost its work with its process: resume that agent, and the watch goes on.'), box),
  })
  await guarded(refreshAgents)
}

// openMail shows an agent's mail, newest first: what other agents sent it,
// read or still waiting, and what it wrote to them. Waiting mail can be
// taken out unread.
async function openMail(a) {
  const list = h('div', { class: 'inbox' })
  const lead = h('p', { class: 'mail-lead' })
  const clear = h('button', { type: 'button', class: 'btn danger small', hidden: true }, 'Discard all waiting')
  const draw = async () => {
    const mails = await guarded(() => api('GET', `/v1/agents/${enc(a.id)}/mail`))
    if (!mails) return
    const waiting = mails.filter((m) => m.state === 'waiting' && !m.sent).length
    lead.textContent = waiting
      ? `${waiting} waiting: it reads one per turn, oldest first, whenever it is idle. Discarded mail is never read.`
      : mails.length ? 'Nothing waits for it.' : 'It has neither sent nor been sent mail.'
    clear.hidden = !waiting
    list.replaceChildren(...mails.map((m) => h('div', { class: 'mail' + (m.sent ? ' sent' : '') },
      h('div', { class: 'mail-head' },
        h('strong', null, m.sent ? `→ ${m.to}` : `← ${m.from}`),
        h('span', { class: 'chip ' + m.state }, m.state),
        m.state === 'waiting' && !m.sent ? h('button', {
          type: 'button', class: 'btn ghost danger', onclick: () => guarded(async () => {
            await api('DELETE', `/v1/agents/${enc(a.id)}/inbox/${m.id}`)
            await draw()
          }),
        }, 'Discard') : null),
      h('div', { class: 'mail-meta' }, `#${m.id} · ${m.sent ? m.to_isolation : m.from_isolation} · ${new Date(m.created_at).toLocaleString()}`),
      h('div', { class: 'mail-text' }, clip(m.text, 8)))))
  }
  clear.addEventListener('click', () => guarded(async () => {
    await api('DELETE', `/v1/agents/${enc(a.id)}/inbox`)
    await draw()
  }))
  await draw()
  await ask({ title: `Mail of "${agentTitle(a)}"`, extra: h('div', null, lead, clear, list), cancel: 'Close' })
  await guarded(refreshAgents)
}

// editAgent changes what an agent is called, its name and its own ID,
// which the daemon checks against every agent's, and its team.
async function editAgent(a) {
  const name = h('input', { class: 'dialog-input', value: a.name, autocomplete: 'off', placeholder: 'from its first message' })
  const id = h('input', { class: 'dialog-input', value: a.handle || '', autocomplete: 'off', autocapitalize: 'off', spellcheck: 'false', placeholder: a.id })
  const [team, teams] = teamInput(a.team)
  let saved = null
  const ok = await ask({
    title: 'Name, ID and team',
    body: `Only what it is called changes; a running turn carries on. Its ID is unique among all agents; left empty, it goes by ${a.id}, the one psps gave it. A team groups agents in the sidebar; a name no team has makes one.`,
    fields: [h('label', { class: 'dialog-field' }, 'Name', name), h('label', { class: 'dialog-field' }, 'ID', id),
      h('label', { class: 'dialog-field' }, 'Team', team, teams)],
    ok: 'Save',
    onOk: async () => {
      const body = {}
      const n = name.value.trim()
      const i = id.value.trim()
      const tm = team.value.trim()
      if (n !== a.name) {
        if (!n) throw new Error('A name is required.')
        body.name = n
      }
      if (i !== (a.handle || '')) body.handle = i
      if (tm !== (a.team || '')) body.team = tm
      if (Object.keys(body).length) saved = await api('PATCH', `/v1/agents/${enc(a.id)}`, body)
    },
  })
  if (!ok || !saved) return
  toast(`"${agentTitle(saved)}" goes by ${shownID(saved)}` + (saved.team ? `, in team ${saved.team}` : ''))
  await guarded(refreshAgents)
}

// openCrons shows an agent's crons, messages sent to it on a schedule, and
// sets them: each in a form of its own, in place of the list. A run goes
// when the agent is idle; one that finds it busy is skipped or waits, as
// the cron says. The list follows the daemon while it is open.
async function openCrons(a) {
  const box = h('div', { class: 'crons' })
  let zone = ''
  let crons = []
  let editing = null // the cron in the form; {} for a new one
  const when = (t) => new Date(t).toLocaleString([], { weekday: 'short', month: 'short', day: 'numeric', hour: '2-digit', minute: '2-digit' })
  const load = async () => {
    const v = await guarded(() => api('GET', `/v1/agents/${enc(a.id)}/crons`))
    if (v) { zone = v.zone; crons = v.crons }
  }
  const changed = async () => { editing = null; await load(); draw(); guarded(refreshAgents) }
  const set = (c, body) => guarded(async () => { await api('PATCH', `/v1/agents/${enc(a.id)}/crons/${c.id}`, body); await changed() })
  const remove = async (c) => {
    if (!await ask({ title: 'Delete this cron?', body: `${describeSchedule(c.schedule)}: its message is not sent again.`, ok: 'Delete', danger: true })) return
    await guarded(async () => { await api('DELETE', `/v1/agents/${enc(a.id)}/crons/${c.id}`); await changed() })
  }
  const row = (c) => h('div', { class: 'cron' + (c.enabled ? '' : ' off') },
    h('div', { class: 'cron-head' },
      h('strong', null, describeSchedule(c.schedule)),
      h('span', { class: 'chip ' + (c.enabled ? 'ok' : '') }, c.enabled ? 'on' : 'off'),
      c.waiting_since ? h('span', { class: 'chip waiting', title: `since ${when(c.waiting_since)}` }, 'waiting for idle') : null,
      c.self ? h('span', { class: 'chip', title: 'The agent set this cron itself, with the cron service' }, 'set by the agent') : null,
      c.set_by ? h('span', { class: 'chip', title: 'An agent set this cron, through the control service' }, `set by ${c.set_by}`) : null,
      c.once ? h('span', { class: 'chip', title: 'Sent the first time it comes, then removed' }, 'once') : null,
      c.compact ? h('span', { class: 'chip', title: 'Each run sends /compact first, and the message once the agent is idle after it' }, '/compact first') : null,
      c.compacted_at ? h('span', { class: 'chip waiting', title: `/compact sent ${when(c.compacted_at)}` }, 'compacting') : null),
    h('div', { class: 'cron-meta' },
      [c.enabled && c.next_at ? `next ${when(c.next_at)}` : null, c.if_busy === 'wait' ? 'waits if busy' : 'skipped if busy'].filter(Boolean).join(' · ')),
    c.last_at ? h('div', { class: 'cron-meta' }, `${when(c.last_at)}: ${c.last_result}` +
      (c.sent_at && c.last_result !== 'sent' ? ` · last sent ${when(c.sent_at)}` : '')) : null,
    h('div', { class: 'cron-text' }, clip(c.prompt, 4)),
    h('div', { class: 'cron-actions' },
      h('button', { type: 'button', class: 'btn small', onclick: () => { editing = c; draw() } }, 'Edit'),
      h('button', { type: 'button', class: 'btn small', onclick: () => set(c, { enabled: !c.enabled }) }, c.enabled ? 'Turn off' : 'Turn on'),
      h('button', { type: 'button', class: 'btn small ghost danger', onclick: () => remove(c) }, 'Delete')))
  const draw = () => {
    // The form has its own Back; the dialog's Close waits for the list.
    box.closest('dialog')?.querySelector(':scope > form > .dialog-actions')?.toggleAttribute('hidden', !!editing)
    if (editing) {
      box.replaceChildren(cronForm(a, editing, zone, changed, () => { editing = null; draw() }))
      return
    }
    box.replaceChildren(
      h('p', { class: 'mail-lead' }, `Times are the Mac's: ${zone}.`),
      crons.length ? h('div', { class: 'cron-list' }, crons.map(row)) : h('p', { class: 'mail-lead' }, 'It has no cron yet.'),
      h('div', null, h('button', { type: 'button', class: 'btn', onclick: () => { editing = {}; draw() } }, '+ New cron')))
  }
  await load()
  draw()
  const ticker = setInterval(async () => { if (!editing) { await load(); if (!editing) draw() } }, 15000)
  await ask({
    title: `Cron of "${agentTitle(a)}"`,
    body: 'Messages sent to it on a schedule, as if you wrote them. A run is sent when the agent is idle; one that finds it busy is skipped, or waits until it is idle, as you choose.',
    extra: box, cancel: 'Close', wide: true,
  })
  clearInterval(ticker)
}

// openTodos is an agent's TODO: prompts put aside for later. Apply takes
// one off the list and into the message being written, after what is
// there already; it is not sent until the operator sends it.
async function openTodos(a) {
  const box = h('div', { class: 'todos' })
  let todos = []
  const load = async () => {
    const v = await guarded(() => api('GET', `/v1/agents/${enc(a.id)}/todos`))
    if (v) todos = v
  }
  const changed = async () => { await load(); draw(); guarded(refreshAgents) }
  const text = h('textarea', { class: 'dialog-input', rows: 3, placeholder: 'A prompt to send it later', 'aria-label': 'New TODO' })
  const err = h('p', { class: 'form-error', role: 'alert', hidden: true })
  const autoNew = h('input', { type: 'checkbox' })
  const autoTitle = 'Sent by itself when a turn ends in idle, oldest first, one a turn'
  const add = async () => {
    if (!text.value.trim()) { text.focus(); return }
    err.hidden = true
    try {
      await api('POST', `/v1/agents/${enc(a.id)}/todos`, { text: text.value, auto: autoNew.checked })
      text.value = ''
      await changed()
      text.focus()
    } catch (e) {
      if (e instanceof Unauthorized) { S.authed = false; render(); return }
      err.textContent = e.message
      err.hidden = false
    }
  }
  text.addEventListener('keydown', (e) => {
    if (e.key === 'Enter' && (e.metaKey || e.ctrlKey) && !e.isComposing) { e.preventDefault(); add() }
  })
  const apply = (t) => guarded(async () => {
    // Taken first: from two pages at once, only one gets it.
    await api('DELETE', `/v1/agents/${enc(a.id)}/todos/${t.id}`)
    box.closest('dialog')?.close('cancel')
    const ta = S.sel === a.id ? document.querySelector('.composer textarea') : null
    const cur = ta ? ta.value : S.drafts.get(a.id) || ''
    const next = cur.trim() ? cur.replace(/\s+$/, '') + '\n\n' + t.text : t.text
    setDraft(a.id, next)
    if (ta) {
      ta.value = next
      ta.dispatchEvent(new Event('input'))
      ta.focus()
      ta.setSelectionRange(next.length, next.length)
    }
    guarded(refreshAgents)
  })
  const remove = async (t) => {
    if (!await ask({ title: 'Delete this TODO?', body: clip(t.text, 3), ok: 'Delete', danger: true })) return
    await guarded(async () => { await api('DELETE', `/v1/agents/${enc(a.id)}/todos/${t.id}`); await changed() })
  }
  const setAuto = (t, on) => guarded(async () => {
    await api('PATCH', `/v1/agents/${enc(a.id)}/todos/${t.id}`, { auto: on })
    await changed()
  })
  const row = (t) => h('div', { class: 'todo' + (t.auto ? ' auto' : '') },
    h('div', { class: 'todo-text' }, clip(t.text, 6)),
    h('div', { class: 'todo-actions' },
      h('label', { class: 'check todo-auto', title: autoTitle },
        h('input', { type: 'checkbox', checked: !!t.auto, onchange: (e) => setAuto(t, e.target.checked) }), 'Send when idle'),
      h('button', { type: 'button', class: 'btn small ghost danger', onclick: () => remove(t) }, 'Delete'),
      h('button', { type: 'button', class: 'btn small primary', title: 'Into the message box, and off the list', onclick: () => apply(t) }, 'Apply')))
  const draw = () => box.replaceChildren(
    todos.length ? h('div', { class: 'todo-list' }, todos.map(row)) : h('p', { class: 'mail-lead' }, 'Nothing put aside yet.'),
    h('div', { class: 'todo-new' }, text, err,
      h('div', { class: 'todo-actions' },
        h('label', { class: 'check todo-auto', title: autoTitle }, autoNew, 'Send when idle'),
        h('button', { type: 'button', class: 'btn', onclick: add }, 'Save'))))
  await load()
  draw()
  queueMicrotask(() => text.focus())
  await ask({
    title: `TODO of "${agentTitle(a)}"`,
    body: 'Prompts put aside to send it later. Apply puts one in the message box and takes it off the list. Send when idle sends it by itself the next time a turn ends, oldest first, one a turn.',
    extra: box, cancel: 'Close', wide: true,
  })
}

// cronForm sets one cron: when, its message, and what a run that finds the
// agent busy does. It lives in the cron dialog's own form, so every button
// is type=button and Enter in a field saves rather than closing it.
function cronForm(a, c, zone, saved, cancel) {
  const p = c.schedule ? scheduleParts(c.schedule) : { kind: 'every', n: 30, unit: 'm' }
  let kind = p.kind
  const n = h('input', { type: 'number', class: 'dialog-input', min: '1', step: '1', value: String(p.kind === 'every' ? p.n : 30), 'aria-label': 'How many' })
  let unit = p.kind === 'every' ? p.unit : 'm'
  const units = h('div', { class: 'segmented' })
  const drawUnits = () => units.replaceChildren(...[['m', 'minutes'], ['h', 'hours']].map(([u, l]) =>
    h('button', { type: 'button', class: u === unit ? 'on' : '', onclick: () => { unit = u; drawUnits() } }, l)))
  drawUnits()
  const time = h('input', { type: 'time', class: 'dialog-input', value: p.kind === 'at' ? `${pad2(p.hour)}:${pad2(p.minute)}` : '09:00', 'aria-label': 'Time' })
  const days = new Set(p.kind === 'at' ? p.days : [1, 2, 3, 4, 5])
  const dayRow = h('div', { class: 'segmented days', role: 'group', 'aria-label': 'Days' })
  const drawDays = () => dayRow.replaceChildren(...DAYS.map(([d, l]) => h('button', {
    type: 'button', class: days.has(d) ? 'on' : '', 'aria-pressed': String(days.has(d)),
    onclick: () => { if (days.has(d)) days.delete(d); else days.add(d); drawDays() },
  }, l)))
  drawDays()
  const expr = h('input', {
    class: 'dialog-input', value: p.kind === 'cron' ? p.expr : '', placeholder: '*/30 9-18 * * 1-5',
    autocomplete: 'off', autocapitalize: 'off', spellcheck: 'false', 'aria-label': 'Cron expression',
  })
  const panes = {
    every: h('div', { class: 'when' }, 'Every', n, units),
    at: h('div', { class: 'when' }, 'At', time, 'on', dayRow),
    cron: h('div', { class: 'when cron-expr' }, expr,
      h('span', { class: 'hint' }, 'minute hour day-of-month month day-of-week, as in crontab: */30 9-18 * * 1-5 is every half hour from 9:00 to 18:30 on weekdays.')),
  }
  const seg = h('div', { class: 'segmented' })
  const drawKind = () => {
    seg.replaceChildren(...[['every', 'Every'], ['at', 'At a time'], ['cron', 'Cron expression']].map(([k, l]) =>
      h('button', { type: 'button', class: k === kind ? 'on' : '', onclick: () => { kind = k; drawKind() } }, l)))
    for (const [k, el] of Object.entries(panes)) el.hidden = k !== kind
  }
  drawKind()
  const prompt = h('textarea', { class: 'dialog-input', value: c.prompt || '', placeholder: 'What to tell it each time', 'aria-label': 'Message' })
  const busy = c.if_busy || 'skip'
  const radio = (value, title, what) => h('label', { class: 'check' },
    h('input', { type: 'radio', name: 'cron-if-busy', value, checked: busy === value }),
    h('span', null, h('strong', null, title), ' — ', what))
  const skip = radio('skip', 'Skip that run', 'it is sent only when it finds the agent idle; the next run tries again.')
  const wait = radio('wait', 'Wait for idle', 'it is sent as soon as the agent is idle; runs that come meanwhile are folded into it.')
  // A cron sent once waits: its one run is not skipped.
  const onceBox = h('input', { type: 'checkbox', checked: !!c.once })
  const once = h('label', { class: 'check' }, onceBox, h('span', null, h('strong', null, 'Once'), ' — it is sent the first time it comes, then removed.'))
  const drawOnce = () => {
    const skipBox = skip.querySelector('input')
    skipBox.disabled = onceBox.checked
    if (onceBox.checked) wait.querySelector('input').checked = true
  }
  onceBox.addEventListener('change', drawOnce)
  drawOnce()
  const compactBox = h('input', { type: 'checkbox', checked: !!c.compact })
  const compact = h('label', { class: 'check' }, compactBox, h('span', null, h('strong', null, 'Compact first'), ' — each run sends /compact, and the message once the agent is idle after it.'))
  const err = h('p', { class: 'form-error dialog-error', role: 'alert', hidden: true })
  const fail = (msg) => { err.textContent = msg; err.hidden = false }
  const save = h('button', { type: 'button', class: 'btn primary' }, c.id ? 'Save' : 'Add cron')
  const go = async () => {
    err.hidden = true
    let spec
    if (kind === 'every') {
      const k = Number(n.value)
      if (!Number.isInteger(k) || k < 1) return fail('Every how many? A whole number, from 1.')
      spec = scheduleSpec({ kind, n: k, unit })
    } else if (kind === 'at') {
      const [hh, mm] = time.value.split(':').map(Number)
      if (!time.value || Number.isNaN(hh) || Number.isNaN(mm)) return fail('At what time?')
      if (!days.size) return fail('Pick a day, at least one.')
      spec = scheduleSpec({ kind, hour: hh, minute: mm, days: [...days] })
    } else {
      spec = expr.value.trim()
      if (!spec) return fail('Write the cron expression.')
    }
    if (!prompt.value.trim()) return fail('Write the message to send.')
    const body = { schedule: spec, prompt: prompt.value, if_busy: wait.querySelector('input').checked ? 'wait' : 'skip', once: onceBox.checked, compact: compactBox.checked }
    save.disabled = true
    try {
      if (c.id) await api('PATCH', `/v1/agents/${enc(a.id)}/crons/${c.id}`, body)
      else await api('POST', `/v1/agents/${enc(a.id)}/crons`, body)
      await saved()
    } catch (e) {
      if (e instanceof Unauthorized) { S.authed = false; render(); return }
      fail(e.message)
      save.disabled = false
    }
  }
  save.addEventListener('click', go)
  const form = h('div', { class: 'cron-form' },
    h('div', { class: 'dialog-field' }, 'When', seg, panes.every, panes.at, panes.cron,
      h('span', { class: 'hint' }, `The Mac's time: ${zone}. Runs missed while the Mac slept or psps was down come due once, as soon as they can.`)),
    h('label', { class: 'dialog-field' }, 'Message', prompt),
    h('div', { class: 'dialog-field' }, 'If the agent is busy when it comes', h('div', { class: 'busy' }, skip, wait)),
    h('div', { class: 'dialog-field' }, h('div', { class: 'busy' }, once, compact)),
    err,
    h('div', { class: 'dialog-actions' }, h('button', { type: 'button', class: 'btn', onclick: cancel }, 'Back'), save))
  form.addEventListener('keydown', (e) => {
    if (e.key === 'Enter' && e.target.matches('input') && !e.isComposing) { e.preventDefault(); if (!save.disabled) go() }
  })
  queueMicrotask(() => (c.id ? prompt : kind === 'cron' ? expr : n).focus())
  return form
}

// stick: the conversation is read at its end, and follows it as events
// come. Scrolled up, by however little, it stays where it was put.
let stick = true

function onConvoScroll(box, id) {
  stick = box.scrollHeight - box.scrollTop - box.clientHeight < 8
  // Drawing the earlier would stop a scroll the buttons began.
  if (box.scrollTop < 400 && Date.now() > goingUntil) {
    const c = convo(id)
    if (c.hidden) showEarlier(c)
    else if (c.more) loadEarlier(id)
  }
  if (!lastSentQueued) { lastSentQueued = true; requestAnimationFrame(() => { lastSentQueued = false; renderLastSent(); renderAskJump() }) }
}
let lastSentQueued = false

// drawn keeps each entry's node, with what it was drawn from: an event
// redraws only the entries it changed. A group of tools is kept under its
// first run.
const drawn = new WeakMap()
// sentBy is the message a node shows, for those the person sent.
const sentBy = new WeakMap()
const isSent = (en) => en.kind === 'user' && !letter(en.text) && !cronMsg(en.text)

// drawnSig is what an entry's view is drawn from, the agent's part included.
function drawnSig(en, a) {
  switch (en.kind) {
    case 'user': {
      const ed = S.edit?.agent === a.id && S.edit.id === en.id && S.edit
      return `user|${rewindable(a)}|${ed ? 'edit:' + !!ed.sending : ''}`
    }
    case 'assistant': return `assistant|${label(a)}|${en.live}|${en.text.length}|${en.text.slice(-32)}`
    case 'tool': return `tool|${en.state}|${en.name}|${en.summary}|${en.output.length}`
    case 'tools': return 'tools|' + en.runs.map((x) => drawnSig(x, a)).join('/')
    case 'approval': return `approval|${en.decision}|${JSON.stringify(en.answers || null)}`
    case 'rewound': return `rewound|${en.entries.length}`
    default: return en.kind
  }
}

function drawEntry(en, a) {
  const key = en.kind === 'tools' ? en.runs[0] : en
  const sig = drawnSig(en, a)
  const was = drawn.get(key)
  if (was && was.sig === sig) return was.node
  const node = entryView(en, a)
  if (node) drawn.set(key, { sig, node })
  if (node && isSent(en)) sentBy.set(node, en)
  return node
}

function renderConvo(forceBottom) {
  const box = document.getElementById('convo')
  const inner = document.getElementById('convo-inner')
  const a = agentById(S.sel)
  if (!box || !inner || !a) return
  if (forceBottom) stick = true
  const c = convo(a.id)
  // A message being edited keeps its caret through a redraw of its own.
  const typing = document.activeElement?.classList.contains('edit-box') ? document.activeElement : null
  const sel = typing && [typing.selectionStart, typing.selectionEnd]
  // Only the latest are drawn; the earlier come as it is scrolled up to them.
  // Scrolled up, the first drawn stays: Safari would not hold the page
  // still as one went above it.
  const all = groupTools(c.entries)
  if (!stick && c.from !== undefined) c.shown = Math.max(c.shown || convoPage, all.length - c.from)
  // What is drawn starts at a message the person sent, so whatever is read
  // has the one it answers drawn, to pin.
  let from = Math.max(0, all.length - (c.shown || convoPage))
  while (from > 0 && !isSent(all[from])) from--
  c.from = from
  c.count = all.length - from
  const items = all.slice(from).map((en) => drawEntry(en, a)).filter(Boolean)
  c.hidden = from
  if (from || c.more) {
    c.earlier ||= h('div', { class: 'earlier', onclick: () => { if (c.hidden) showEarlier(c); else loadEarlier(a.id) } })
    c.earlier.textContent = from ? `${from} earlier` : c.loadingEarlier ? 'Loading earlier…' : 'Earlier'
    items.unshift(c.earlier)
  }
  if (!items.length) items.push(h('div', { class: 'empty' }, c.ready ? 'Nothing yet: the first message you send starts it.' : 'Loading…'))
  // While it works, three dots under its last word, where its next comes;
  // not under a reply it is writing, which has a cursor of its own.
  const last = c.entries[c.entries.length - 1]
  if (c.ready && a.state?.status === 'working' && !a.archived_at && !(last?.kind === 'assistant' && last.live)) {
    c.typing ||= h('div', { class: 'typing-row', role: 'status', 'aria-label': `${agentTitle(a)} is working` },
      h('span', { class: 'typing' }, h('i'), h('i'), h('i')))
    items.push(c.typing)
  }
  // Nodes already in place stay, so what is read does not move. Those
  // gone go first: the rest are then in order, and none is moved.
  const keep = new Set(items)
  for (const node of [...inner.children]) if (!keep.has(node)) node.remove()
  let at = inner.firstChild
  for (const node of items) {
    if (node === at) at = at.nextSibling
    else inner.insertBefore(node, at)
  }
  const ta = typing && inner.querySelector('.edit-box')
  if (ta && ta !== typing) { ta.focus({ preventScroll: true }); ta.setSelectionRange(...sel) }
  foldLong()
  if (stick) box.scrollTop = box.scrollHeight
  renderLastSent()
  renderAskJump()
}

// A message the person sent that the screen shows as longFold lines or more
// is folded to its first foldPeek, and opens on a click anywhere on it. The
// lines are as drawn, in the bubble's own line height: a paragraph with no
// break in it folds once it wraps that far, and a narrower window folds
// more. An open one folds again only from its button, so selecting its
// text never folds it.
const longFold = 20
const foldPeek = 8
// foldWidth is the width each bubble was measured at, so a redraw measures
// only those new or rewrapped.
const foldWidth = new WeakMap()
let foldSeen = null
const foldResize = typeof ResizeObserver === 'function' ? new ResizeObserver(() => foldLong()) : null

function foldLong() {
  const box = document.getElementById('convo')
  if (!box) return
  if (foldResize && foldSeen !== box) { foldResize.disconnect(); foldResize.observe(box); foldSeen = box }
  // Every measure first, then every change, so the layout is worked out once.
  const todo = []
  for (const b of box.querySelectorAll('.msg-user[data-fold]')) {
    const body = b.firstElementChild
    const w = body.clientWidth
    if (!w || foldWidth.get(b) === w) continue
    const lh = parseFloat(getComputedStyle(body).lineHeight) || 22.5
    todo.push([b, w, Math.round(body.scrollHeight / lh)])
  }
  for (const [b, w, lines] of todo) {
    foldWidth.set(b, w)
    setFold(b, lines)
  }
}

function setFold(b, lines) {
  const long = lines >= longFold
  const folded = long && !S.openLong.has(b.dataset.fold)
  b.classList.toggle('long', long)
  b.classList.toggle('folded', folded)
  b.querySelector(':scope > .fold-btn')?.remove()
  b.onclick = folded ? (e) => toggleFold(e, b, lines) : null
  if (!long) return
  b.style.setProperty('--peek', foldPeek)
  b.append(h('button', {
    type: 'button', class: 'fold-btn', 'aria-expanded': String(!folded), onclick: (e) => toggleFold(e, b, lines),
  }, icon('chevron-down'), folded ? 'Show all' : 'Show less', folded ? h('span', { class: 'n' }, ` · ${lines} lines`) : null))
}

function toggleFold(e, b, lines) {
  e.stopPropagation()
  // A click that ends a selection of the folded text is not one to open it.
  if (b.classList.contains('folded') && e.target === b.firstElementChild && String(getSelection()).length) return
  const key = b.dataset.fold
  const wasOpen = S.openLong.has(key)
  if (wasOpen) S.openLong.delete(key)
  else S.openLong.add(key)
  stick = false
  setFold(b, lines)
  // Folding a long message back keeps its top in view rather than leaving the reader below it.
  const box = document.getElementById('convo')
  if (wasOpen && b.getBoundingClientRect().top < box.getBoundingClientRect().top) b.scrollIntoView({ block: 'start' })
  b.querySelector(':scope > .fold-btn')?.focus({ preventScroll: true })
}

const convoPage = 150

// renderLastSent pins, above the conversation, a line of the message the
// person sent that what is read answers: the last one gone up past the top.
// It takes them back to it.
function renderLastSent() {
  const bar = document.getElementById('last-sent')
  const inner = document.getElementById('convo-inner')
  if (!bar || !inner) return
  const line = document.getElementById('convo').getBoundingClientRect().top + 60
  let node = null
  for (let n = inner.lastElementChild; n; n = n.previousElementSibling) {
    if (sentBy.has(n) && n.getBoundingClientRect().top < line) { node = n; break }
  }
  bar.hidden = !node
  if (!node) return
  const en = sentBy.get(node)
  const btn = bar.querySelector('.last-sent')
  const text = en.text.replace(/\s+/g, ' ').trim()
  if (btn.textContent !== text) { btn.textContent = text; btn.title = en.text }
  btn.onclick = () => node.scrollIntoView({ block: 'start', behavior: 'smooth' })
  const c = convo(S.sel)
  bar.querySelector('[data-go=up]').disabled = !sentNear(-1) && !c.hidden && !c.more
  bar.querySelector('[data-go=down]').disabled = !sentNear(1)
  const box = document.getElementById('convo')
  bar.querySelector('[data-go=end]').disabled = box.scrollHeight - box.scrollTop - box.clientHeight < 8
}

// sentNear is the message the person sent before (-1) or after (1) where
// one comes to rest when gone to, under the pinned line.
function sentNear(dir) {
  const inner = document.getElementById('convo-inner')
  const box = document.getElementById('convo')
  // At the top, what is above where one rests is already in view.
  if (dir < 0 && box.scrollTop < 2) return null
  const rest = box.getBoundingClientRect().top + 56
  const sent = [...inner.children].filter((n) => sentBy.has(n))
  return dir < 0
    ? sent.findLast((n) => n.getBoundingClientRect().top < rest - 12)
    : sent.find((n) => n.getBoundingClientRect().top > rest + 12)
}

let goingUntil = 0

function goSent(dir) {
  const c = convo(S.sel)
  // Before the first drawn, the earlier are drawn: they start at one.
  if (dir < 0 && !sentNear(-1) && !c.hidden && c.more) { loadEarlier(S.sel).then((ok) => ok && goSent(dir)); return }
  if (dir < 0 && !sentNear(-1) && c.hidden) showEarlier(c)
  const node = sentNear(dir)
  if (!node) return
  goingUntil = Date.now() + 1000
  node.scrollIntoView({ block: 'start', behavior: 'smooth' })
  const id = S.sel
  setTimeout(() => { const box = document.getElementById('convo'); if (box && S.sel === id) onConvoScroll(box, id) }, 1050)
}

// showEarlier draws a page more of the conversation above, the one read
// staying where it was.
function showEarlier(c) {
  const box = document.getElementById('convo')
  const below = box.scrollHeight - box.scrollTop
  c.shown = (c.count || convoPage) + convoPage
  renderConvo()
  box.scrollTop = box.scrollHeight - below
}

// groupTools folds a run of one tool on one thing in a row (five Edits of
// the same file) into a 'tools' entry, shown folded until opened.
function groupTools(entries) {
  const out = []
  for (const en of entries) {
    const prev = out[out.length - 1]
    const first = prev?.kind === 'tools' ? prev.runs[0] : prev
    if (en.kind === 'tool' && en.name && first?.kind === 'tool' && first.name === en.name && first.summary === en.summary) {
      if (prev.kind === 'tools') prev.runs.push(en)
      else out[out.length - 1] = { kind: 'tools', runs: [prev, en] }
      continue
    }
    out.push(en)
  }
  return out
}

// stamp is a message's time, MM-DD HH:mm local; none for an event the
// daemon gave no time.
function stamp(time) {
  const d = new Date(time)
  if (!time || isNaN(d) || d.getFullYear() < 2000) return null
  const p = (n) => String(n).padStart(2, '0')
  return h('time', { class: 'stamp', datetime: d.toISOString(), title: d.toLocaleString() },
    `${p(d.getMonth() + 1)}-${p(d.getDate())} ${p(d.getHours())}:${p(d.getMinutes())}`)
}

// entryView draws one entry; frozen ones are an earlier version, there to
// read only.
const toolMarks = { ok: 'check', failed: 'x', running: 'ellipsis' }

function entryView(en, a, frozen) {
  switch (en.kind) {
    case 'user': {
      const l = letter(en.text)
      if (l) return h('div', { class: 'msg-mail' }, h('div', { class: 'who' }, icon('mail'), ` Mail #${l.id} from ${l.from} · ${l.iso}`), h('div', { class: 'text' }, markdown(l.body)))
      const rc = receiptsMsg(en.text)
      if (rc) return h('div', { class: 'msg-cron msg-receipt' }, h('div', { class: 'who' }, icon('target'), ' From psps: what came of what it sent'), h('div', { class: 'text' }, richText(rc.body)))
      const cm = cronMsg(en.text)
      if (cm) return h('div', { class: 'msg-cron' }, h('div', { class: 'who' }, icon('clock'), ` Cron #${cm.id}${cm.self ? ' (the agent\'s own)' : cm.by ? ` (set by ${cm.by})` : ''} · ${describeSchedule(cm.schedule)}`), h('div', { class: 'text' }, richText(cm.body)))
      if (!frozen && S.edit?.agent === a.id && S.edit.id === en.id) return editView(en, a)
      const bubble = h('div', { class: 'msg-user text', 'data-fold': `${a.id}:${en.id || en.time}` }, h('div', { class: 'msg-body' }, richText(en.text)))
      // One copied from the agent forked (src below zero) is not its CLI's to go back to.
      const edit = !frozen && en.id && en.src > 0 && rewindable(a)
        ? h('button', { class: 'edit-msg', title: 'Edit and resend: the agent goes back to here', 'aria-label': 'Edit message', onclick: () => startEdit(a, en) }, icon('pencil'))
        : null
      return h('div', { class: 'msg-user-row' }, edit, stamp(en.time), bubble)
    }
    case 'rewound': {
      const key = `${a.id}:${en.src}`
      const n = en.entries.filter((x) => x.kind === 'user' || x.kind === 'assistant').length
      return h('details', {
        class: 'rewound', open: S.openRewound.has(key),
        ontoggle: (e) => { if (e.target.open) S.openRewound.add(key); else S.openRewound.delete(key) },
      },
        h('summary', null, icon('archive-restore'), ` Edited · the earlier version (${n} message${n === 1 ? '' : 's'}) is gone from the agent's memory`),
        h('div', { class: 'rewound-body' }, groupTools(en.entries).map((x) => entryView(x, a, true)).filter(Boolean)))
    }
    case 'assistant':
      return h('div', { class: 'msg-agent' + (en.live ? ' live' : '') }, h('div', { class: 'who' }, label(a), stamp(en.time)), h('div', { class: 'text' }, markdown(en.text, en.live)))
    case 'reasoning': return h('div', { class: 'reasoning' }, en.text)
    case 'tool': {
      const mark = icon(toolMarks[en.state])
      const out = (en.output || '').trim()
      // The summary line is cut to the width; opened, the call's input is
      // shown whole above its output, set apart from it (see .tool .tin).
      const shell = en.name === 'Bash' || en.name === 'shell'
      return h('details', { class: 'tool' },
        h('summary', null, h('span', { class: 'mark ' + en.state }, mark), h('span', { class: 'tname' }, en.name), h('span', { class: 'tsum' }, en.summary)),
        en.summary ? h('pre', { class: 'tin' + (shell ? ' shell' : '') }, en.summary) : null,
        out ? h('pre', { class: 'tout' }, clip(out, 40)) : null)
    }
    case 'tools': {
      const first = en.runs[0]
      const key = `${a.id}:${first.item}`
      const states = en.runs.map((x) => x.state)
      const state = states.includes('failed') ? 'failed' : states.includes('running') ? 'running' : 'ok'
      return h('details', {
        class: 'tool tools', open: S.openTools.has(key),
        ontoggle: (e) => { if (e.target.open) S.openTools.add(key); else S.openTools.delete(key) },
      },
        h('summary', null, h('span', { class: 'mark ' + state }, icon(toolMarks[state])),
          h('span', { class: 'tname' }, first.name), h('span', { class: 'tsum' }, first.summary), h('span', { class: 'tcount' }, `×${en.runs.length}`)),
        h('div', { class: 'tools-body' }, en.runs.map((x) => entryView(x, a, frozen))))
    }
    case 'approval': {
      const d = en.decision
      if (!d && !frozen) return en.questions ? questionCard(a, en) : approvalCard(a, en)
      const outcome = d ? ' — ' + decisionText(d) : ''
      return h('div', { class: 'approval' + (d ? ' decided' : '') },
        h('div', { class: 'head' }, `Approval: ${en.akind}${outcome}`),
        h('div', { class: 'what' }, en.summary),
        en.reason && !d ? h('div', { class: 'why' }, en.reason) : null,
        (en.questions || []).filter((q) => en.answers && q.question in en.answers)
          .map((q) => h('div', { class: 'why' }, `${q.question} → ${en.answers[q.question]}`)))
    }
    case 'interrupted': return h('div', { class: 'interrupted' }, icon('pause'), ' This turn was interrupted. Send a message to continue.')
    case 'error': return h('div', { class: 'error-line' }, icon('x'), ' ' + en.text)
    case 'notice': return h('div', { class: 'notice' }, en.text)
  }
  return null
}

function editView(en, a) {
  const ed = S.edit
  const ta = h('textarea', { class: 'edit-box', rows: 1, value: ed.text, 'aria-label': 'Edited message' })
  const grow = () => { ta.style.height = 'auto'; ta.style.height = Math.min(ta.scrollHeight, 320) + 'px' }
  ta.addEventListener('input', () => { ed.text = ta.value; grow() })
  ta.addEventListener('keydown', (e) => {
    if (e.key === 'Escape') { e.preventDefault(); cancelEdit() }
    if (e.key === 'Enter' && !e.shiftKey && !coarse && !e.isComposing) { e.preventDefault(); resend(a, en) }
  })
  queueMicrotask(grow)
  const c = convo(a.id)
  const later = c.entries.slice(c.entries.indexOf(en) + 1).filter((x) => x.kind === 'user')
  const mails = later.filter((x) => letter(x.text)).length
  const crons = later.filter((x) => cronMsg(x.text)).length
  const plural = (n, what) => `${n} ${what}${n === 1 ? '' : 's'}`
  const gone = [mails && plural(mails, 'mail'), crons && plural(crons, 'cron message')].filter(Boolean).join(' and ')
  const ok = rewindable(a)
  const note = !ok ? 'The agent is busy; resend once it is idle, or stop it.'
    : 'The agent forgets this message and everything after it; files stay as they are.' +
      (gone ? ` That includes ${gone}, not sent again.` : '')
  return h('div', { class: 'msg-edit' }, ta,
    h('div', { class: 'note' }, note),
    h('div', { class: 'actions' },
      h('button', { class: 'btn small', onclick: cancelEdit }, 'Cancel'),
      h('button', { class: 'btn small primary', disabled: !ok || ed.sending || !ed.text.trim(), onclick: () => resend(a, en) }, ed.sending ? 'Sending…' : 'Resend')))
}

// approvalCard is what an agent asks to do (run a command, change files),
// drawn in the conversation where it asked, like a question: there it
// scrolls with the rest instead of covering a phone's screen.
function approvalCard(a, ap) {
  return h('div', { class: 'approval asking' },
    h('div', { class: 'head' }, `${label(a)} asks to ${askText(ap.akind)}`),
    h('div', { class: 'what' }, ap.summary),
    ap.akind === 'ssm' && ap.detail ? h('pre', { class: 'detail' }, ap.detail) : null,
    ap.reason ? h('div', { class: 'why' }, ap.reason) : null,
    h('div', { class: 'actions' },
      h('button', { class: 'btn primary', onclick: () => decide(ap.id, 'accept') }, 'Accept'),
      h('button', { class: 'btn', onclick: () => decide(ap.id, 'decline') }, 'Decline'),
      h('button', { class: 'btn danger', onclick: () => decide(ap.id, 'cancel') }, 'Decline & stop')))
}

// questionCard is a question the agent asks (Claude's AskUserQuestion),
// drawn in the conversation where it was asked: each of its questions with
// its options to pick, one or several, and a box for an answer of one's
// own. The card stays while it waits, so the conversation keeps it through
// redraws; a pick redraws only the card. What is picked lives in S, kept
// when the agent is left and come back to; a box being typed in keeps its
// caret.
function questionCard(a, ap) {
  const card = h('div', { class: 'approval asking' })
  const st = (S.answering[ap.id] ||= { picked: {}, own: {}, cur: 0 })
  const answerTo = (q) => {
    const own = (st.own[q.question] || '').trim()
    if (!q.multi_select) return own || st.picked[q.question] || ''
    return [...(st.picked[q.question] || []), ...(own ? [own] : [])].join(', ')
  }
  let draw = () => {
    const typing = card.contains(document.activeElement) && document.activeElement.dataset?.q != null ? document.activeElement : null
    const sel = typing && [typing.dataset.q, typing.selectionStart, typing.selectionEnd]
    const answer = h('button', { class: 'btn primary' }, 'Answer')
    const ready = () => { answer.disabled = !ap.questions.every((q) => answerTo(q)) }
    answer.addEventListener('click', () => {
      const answers = Object.fromEntries(ap.questions.map((q) => [q.question, answerTo(q)]))
      decide(ap.id, 'accept', answers)
    })
    // What the picked options mean, under them: a touch screen has no
    // hover for an option's title.
    const descs = (q) => {
      const picked = st.picked[q.question]
      const on = q.multi_select ? picked || [] : picked ? [picked] : []
      const shown = (q.options || []).filter((o) => o.description && on.includes(o.label))
      return shown.map((o) => h('div', { class: 'opt-desc' }, q.multi_select || shown.length > 1 ? h('b', null, o.label + ': ') : null, o.description))
    }
    const question = (q, i) => {
      const picked = st.picked[q.question]
      // Where one may be picked, words of one's own are the answer instead:
      // the option is shown unpicked while there are any.
      const typed = () => !q.multi_select && !!(st.own[q.question] || '').trim()
      const isOn = (o) => q.multi_select ? (picked || []).includes(o.label) : picked === o.label && !typed()
      const opts = (q.options || []).map((o, j) => {
        const on = isOn(o)
        return h('button', {
          class: 'btn small opt' + (on ? ' on' : ''), title: o.description || '', 'aria-pressed': String(on),
          onclick: () => {
            const on = isOn(o)
            st.cur = i
            if (!q.multi_select && on) delete st.picked[q.question]
            else if (!q.multi_select) { st.picked[q.question] = o.label; st.own[q.question] = '' }
            else st.picked[q.question] = on ? picked.filter((l) => l !== o.label) : [...(picked || []), o.label]
            draw()
          },
        }, h('span', { class: 'opt-key' }, String(j + 1)), o.label)
      })
      const notes = descs(q)
      // Typing changes them in place: a redraw would take the box away from
      // under an IME's composition.
      const shade = () => {
        if (q.multi_select) return
        ;(q.options || []).forEach((o, j) => {
          const on = isOn(o)
          opts[j].classList.toggle('on', on)
          opts[j].setAttribute('aria-pressed', String(on))
        })
        for (const n of notes) n.hidden = typed()
      }
      const own = h('input', { class: 'own', value: st.own[q.question] || '', placeholder: q.multi_select ? 'And in your own words…' : 'Or in your own words…', autocomplete: 'off', 'aria-label': q.question })
      own.dataset.q = String(i)
      shade()
      own.addEventListener('input', () => { st.own[q.question] = own.value; shade(); ready() })
      own.addEventListener('focus', () => { st.cur = i; markCur() })
      own.addEventListener('keydown', (e) => {
        if (e.isComposing) return
        if (e.key === 'Enter' && !answer.disabled) answer.click()
        if (e.key === 'Escape') { e.preventDefault(); own.blur() }
      })
      return h('div', { class: 'q' },
        h('div', { class: 'q-head' }, q.header ? h('span', { class: 'chip' }, q.header) : null, q.question,
          q.multi_select ? h('span', { class: 'hint' }, ' (any of them)') : null),
        opts.length ? h('div', { class: 'opts' }, opts) : null, ...notes, own)
    }
    card.replaceChildren(
      h('div', { class: 'head' }, `${label(a)} asks you`),
      ...ap.questions.map(question),
      h('div', { class: 'actions' },
        answer,
        h('button', { class: 'btn', onclick: () => decide(ap.id, 'decline') }, 'Decline'),
        h('button', { class: 'btn danger', onclick: () => decide(ap.id, 'cancel') }, 'Decline & stop')))
    ready()
    if (sel) {
      const el = card.querySelector(`input[data-q="${sel[0]}"]`)
      if (el) { el.focus({ preventScroll: true }); el.setSelectionRange(sel[1], sel[2]) }
    }
  }
  // The question the keys answer is marked where there are several.
  const markCur = () => card.querySelectorAll('.q').forEach((el, i) => el.classList.toggle('cur', ap.questions.length > 1 && i === st.cur))
  const rows = () => card.querySelectorAll('.q')
  // The keys, taken where no field has them (see the shortcuts): a number
  // picks that option of the question at hand, going on to the next where
  // only one may be picked; 0 writes an answer of one's own; Enter answers.
  card.keys = {
    pick(n) {
      const q = ap.questions[st.cur]
      const opt = rows()[st.cur]?.querySelectorAll('.opt')[n - 1]
      if (!opt) return
      opt.click()
      if (!q.multi_select && st.picked[q.question] && st.cur < ap.questions.length - 1) { st.cur++; draw() }
      rows()[st.cur]?.scrollIntoView({ block: 'nearest' })
    },
    write() {
      const own = rows()[st.cur]?.querySelector('input.own')
      own?.focus()
      own?.scrollIntoView({ block: 'nearest' })
    },
    answer() { card.querySelector('.actions .btn.primary:not(:disabled)')?.click() },
  }
  const drawn = draw
  draw = () => { drawn(); markCur() }
  draw()
  return card
}

// renderAskJump shows, over the message box, a way down to the first
// question or approval still waiting below what is in view.
function renderAskJump() {
  const btn = document.getElementById('ask-jump')
  const box = document.getElementById('convo')
  if (!btn || !box) return
  const bottom = box.getBoundingClientRect().bottom - 40
  const card = [...box.querySelectorAll('.approval.asking')].find((c) => c.getBoundingClientRect().top >= bottom)
  btn.hidden = !card
  if (!card) return
  btn.lastChild.textContent = card.querySelector('.q') ? 'A question waits for you' : 'An approval waits for you'
  btn.onclick = () => {
    card.scrollIntoView({ block: 'start', behavior: 'smooth' })
    card.classList.remove('flash')
    void card.offsetWidth
    card.classList.add('flash')
  }
}

// ---- Completion in the message box: commands typed at the start of a
// message (/; psps's own, then the agent CLI's), Codex's skills anywhere in
// one ($), and files of the agent's workspace (@).

// fuzzy scores how well q matches s, its letters in order: higher is
// better, -1 not at all. Runs of letters and the starts of words count.
function fuzzy(q, s) {
  if (!q) return 0
  const t = s.toLowerCase()
  q = q.toLowerCase()
  let score = 0, j = 0, run = 0, prev = -2
  for (let i = 0; i < t.length && j < q.length; i++) {
    if (t[i] !== q[j]) continue
    run = i === prev + 1 ? run + 1 : 0
    score += 1 + run * 2 + (i === 0 || '/._- '.includes(t[i - 1]) ? 3 : 0)
    prev = i
    j++
  }
  return j < q.length ? -1 : score - t.length / 100
}

// The agent's commands and files as last fetched: fetched again when
// older than ttl, once at a time, while the last ones are shown. ready is
// the fetch under way.
const completions = { commands: new Map(), files: new Map() }
function fetched(kind, id, ttl) {
  const cache = completions[kind]
  let c = cache.get(id)
  if (!c) cache.set(id, (c = { at: 0, data: null, error: null, ready: null }))
  if (!c.ready && Date.now() - c.at >= ttl) {
    c.ready = api('GET', `/v1/agents/${id}/${kind}`)
      .then((d) => { c.data = d; c.error = null }, (e) => { c.error = e })
      .finally(() => { c.at = Date.now(); c.ready = null })
  }
  return c
}

// triggered is what the text before the caret asks to complete, if
// anything: the token from from to the caret, q being what is typed of it.
function triggered(a, before) {
  let m = /^\/(\S*)$/.exec(before)
  if (m) return { kind: 'command', prefix: '/', q: m[1], from: 0 }
  m = /^\/(\S+) (\S*)$/.exec(before)
  if (m) return { kind: 'arg', name: m[1], q: m[2], from: before.length - m[2].length }
  m = /(^|\s)([@$])(\S*)$/.exec(before)
  if (m && m[2] === '@') return { kind: 'file', q: m[3], from: before.length - m[3].length - 1 }
  if (m && a.kind === 'codex') return { kind: 'command', prefix: '$', q: m[3], from: before.length - m[3].length - 1 }
  return null
}

// fileItems are the files and folders that match q: a folder's own
// entries while q is one (or nothing), else the best matches anywhere.
function fileItems(files, q) {
  const dirs = new Set()
  for (const f of files) {
    for (let i = f.indexOf('/'); i >= 0; i = f.indexOf('/', i + 1)) dirs.add(f.slice(0, i + 1))
  }
  const all = [...dirs, ...files]
  if (q === '' || q.endsWith('/')) {
    return all.filter((p) => p.startsWith(q) && p !== q && !p.slice(q.length, -1).includes('/'))
      .sort((x, y) => (y.endsWith('/') - x.endsWith('/')) || x.localeCompare(y)).slice(0, 50)
  }
  const scored = []
  for (const p of all) {
    const base = p.slice(p.lastIndexOf('/', p.length - 2) + 1)
    // Its own name matching counts for more.
    const b = q.includes('/') ? -1 : fuzzy(q, base)
    const s = Math.max(fuzzy(q, p), b < 0 ? -1 : b + 4)
    if (s >= 0) scored.push([s, p])
  }
  return scored.sort((x, y) => y[0] - x[0] || x[1].length - y[1].length).slice(0, 50).map(([, p]) => p)
}

// completer drives the box of choices over ta. accept(send) is called with
// send true when a choice made the message a whole command, to send it.
function completer(a, ta, submit) {
  const box = h('div', { class: 'complete', role: 'listbox', hidden: true })
  let items = [], note = null, active = 0, at = null, dismissed = null

  const close = () => { box.hidden = true; items = []; at = null }
  const show = (rows, why) => {
    items = rows
    note = why
    active = Math.min(active, Math.max(rows.length - 1, 0))
    // replaceChildren turns null into the text "null"; keep only nodes.
    box.replaceChildren(...[
      ...rows.map((it, i) => h('div', {
        class: 'complete-item' + (i === active ? ' active' : '') + (it.file ? ' file' : '') + (it.dir ? ' dir' : ''),
        role: 'option', 'aria-selected': i === active ? 'true' : 'false',
        onmousedown: (e) => { e.preventDefault(); active = i; accept(false) },
      },
        h('span', { class: 'ci-name' }, it.label, it.hint ? h('span', { class: 'ci-hint' }, ' ' + it.hint) : null),
        it.desc ? h('span', { class: 'ci-desc' }, it.desc) : null,
        it.psps ? h('span', { class: 'ci-tag', title: 'psps does this itself; the agent is not sent it' }, 'psps') : null)),
      note ? h('div', { class: 'complete-note' }, note) : null,
    ].filter(Boolean))
    box.hidden = !rows.length && !note
    box.querySelector('.active')?.scrollIntoView({ block: 'nearest' })
  }

  const update = () => {
    if (ta.selectionStart !== ta.selectionEnd || a.archived_at) return close()
    const t = triggered(a, ta.value.slice(0, ta.selectionStart))
    if (!t || (dismissed && dismissed.from === t.from && dismissed.kind === t.kind)) return close()
    dismissed = null
    // A word just begun asks again: the files may have changed since. Typing
    // on in it does not.
    const fresh = !at || at.from !== t.from || at.kind !== t.kind
    if (fresh) active = 0
    at = t
    const c = fetched(t.kind === 'file' ? 'files' : 'commands', a.id, !fresh ? Infinity : t.kind === 'file' ? 0 : 15000)
    // What comes back is shown as it comes, in place of what was.
    c.ready?.then(() => { if (at === t) update() })
    if (!c.data) {
      if (c.error) return show([], t.kind === 'file' ? `Can't list its files: ${c.error.message}` : null)
      return show([], t.kind === 'file' ? 'Listing files…' : null)
    }
    if (t.kind === 'file') {
      const rows = fileItems(c.data.files, t.q).map((p) => ({ label: p, value: '@' + p, dir: p.endsWith('/'), file: true }))
      return show(rows, c.data.truncated ? `Only the first ${c.data.files.length} files are searched.`
        : rows.length ? null : c.ready ? 'Listing files…' : 'No such file.')
    }
    const cmds = c.data.commands || []
    if (t.kind === 'arg') {
      const cmd = cmds.find((x) => x.prefix === '/' && x.name === t.name)
      const rows = (cmd?.args || [])
        .map((v) => [fuzzy(t.q, v.value), v]).filter(([s]) => s >= 0)
        .sort((x, y) => (t.q ? y[0] - x[0] : 0))
        .map(([, v]) => ({ label: v.value, value: v.value, desc: v.description, whole: true }))
      return show(rows)
    }
    const rows = cmds.filter((x) => x.prefix === t.prefix)
      .map((x) => [fuzzy(t.q, x.name), x]).filter(([s]) => s >= 0)
      // Typed: the best match first; not yet: psps's own, then the agent's.
      .sort((x, y) => (t.q ? y[0] - x[0] : 0))
      .map(([, x]) => ({ label: x.prefix + x.name, value: x.prefix + x.name, hint: x.hint, desc: x.description, psps: x.psps,
        whole: x.prefix === '/' && !x.hint && !x.args, typed: x.name === t.q }))
    show(rows)
  }

  // accept puts the active choice in place of what is typed of it. send:
  // Enter, which sends a message the choice made a whole command.
  const accept = (send) => {
    const it = items[active]
    if (!it || !at) return false
    const caret = ta.selectionStart
    let end = caret
    while (end < ta.value.length && !/\s/.test(ta.value[end])) end++
    const after = ta.value.slice(end)
    const fill = it.value + (it.dir ? '' : ' ')
    ta.value = ta.value.slice(0, at.from) + fill + after.replace(/^ /, '')
    const pos = at.from + fill.length
    ta.setSelectionRange(pos, pos)
    // Which also completes what comes next: a command's argument, a folder's
    // files.
    ta.dispatchEvent(new Event('input'))
    if (send && (it.whole || it.typed) && /^\/\S+( \S+)?\s*$/.test(ta.value)) {
      close()
      submit()
    }
    return true
  }

  // key handles a key the box takes; true: it did.
  const key = (e) => {
    if (box.hidden || e.isComposing) return false
    switch (e.key) {
      case 'ArrowDown': case 'ArrowUp': {
        if (!items.length) return false
        e.preventDefault()
        active = (active + (e.key === 'ArrowDown' ? 1 : items.length - 1)) % items.length
        show(items, note)
        return true
      }
      case 'Tab':
        if (!items.length || e.shiftKey) return false
        e.preventDefault()
        return accept(false)
      case 'Enter':
        if (!items.length || e.shiftKey) return false
        e.preventDefault()
        return accept(!coarse)
      case 'Escape':
        e.preventDefault()
        dismissed = at
        close()
        return true
    }
    return false
  }

  ta.addEventListener('input', update)
  ta.addEventListener('click', update)
  ta.addEventListener('keyup', (e) => { if (['ArrowLeft', 'ArrowRight', 'Home', 'End'].includes(e.key)) update() })
  ta.addEventListener('blur', close)
  return { box, key }
}

function composer(a) {
  const ta = h('textarea', {
    rows: 1,
    placeholder: a.archived_at ? 'This agent is archived.' : a.name ? 'Message the agent… / for commands, @ for files' : 'What should the agent do?',
    title: coarse ? null : 'Enter sends · Shift+Enter adds a line · / commands · @ files' + (a.kind === 'codex' ? ' · $ skills' : ''),
    disabled: !!a.archived_at,
    value: S.drafts.get(a.id) || '',
  })
  // An empty box is one line: its scrollHeight would count the
  // placeholder's lines where it wraps, as on a phone.
  const grow = () => {
    ta.style.height = ''
    if (!ta.value) return
    ta.style.height = 'auto'
    ta.style.height = Math.min(ta.scrollHeight, 200) + 'px'
  }
  const submit = async () => {
    const text = ta.value.trim()
    if (!text) return
    ta.value = ''
    setDraft(a.id, '')
    grow()
    await send(text)
    // Its first message names it; show that now, not at the next poll.
    if (!agentById(a.id)?.name) await guarded(refreshAgents)
  }
  ta.addEventListener('input', () => { setDraft(a.id, ta.value); grow() })
  const done = completer(a, ta, submit)
  ta.addEventListener('keydown', (e) => {
    if (done.key(e)) return
    if (e.key === 'Enter' && !e.shiftKey && !coarse && !e.isComposing) { e.preventDefault(); submit() }
    if (e.key === 'Escape') { S.settingsOpen = false; renderSettings(a) }
  })
  queueMicrotask(grow)
  const wrap = h('div', { class: 'composer' },
    h('div', { id: 'settings-slot' }),
    h('div', { id: 'tasks-slot' }),
    h('div', { id: 'status-slot', class: 'status-line' }),
    h('div', { class: 'composer-row' },
      done.box,
      ta,
      h('button', {
        class: 'icon-btn' + (S.settingsOpen ? ' on' : ''), 'aria-label': 'Agent settings', title: 'Agent settings',
        disabled: !!a.archived_at, onclick: () => { S.settingsOpen = !S.settingsOpen; renderSettings(a) },
      }, icon('settings')),
      sendButton(a, ta, submit)))
  queueMicrotask(() => { renderSettings(a); renderStatusLine() })
  return wrap
}

// sendButton sends on a click; held down, it offers to put the message
// aside in the agent's TODO instead.
function sendButton(a, ta, submit) {
  let timer = 0
  let held = false
  const menu = h('div', { class: 'send-menu', hidden: true })
  const close = () => { menu.hidden = true; document.removeEventListener('pointerdown', outside, true) }
  const outside = (e) => { if (!wrap.contains(e.target)) close() }
  const todo = (auto) => guarded(async () => {
    close()
    const text = ta.value.trim()
    if (!text) { ta.focus(); return }
    await api('POST', `/v1/agents/${enc(a.id)}/todos`, { text, auto })
    ta.value = ''
    setDraft(a.id, '')
    ta.dispatchEvent(new Event('input'))
    toast(auto ? 'Put aside in TODO, sent when a turn ends' : 'Put aside in TODO')
    guarded(refreshAgents)
  })
  menu.append(
    h('button', { type: 'button', class: 'btn primary send-btn', title: 'Sent by itself when a turn ends in idle', onclick: () => todo(true) }, 'Auto'),
    h('button', { type: 'button', class: 'btn primary send-btn', onclick: () => todo(false) }, 'TODO'))
  const btn = h('button', { class: 'btn primary send-btn', disabled: !!a.archived_at, title: 'Hold for TODO' }, 'Send')
  btn.addEventListener('pointerdown', (e) => {
    if (e.button !== 0) return
    held = false
    clearTimeout(timer)
    timer = setTimeout(() => {
      held = true
      menu.hidden = false
      document.addEventListener('pointerdown', outside, true)
    }, 500)
  })
  for (const ev of ['pointerup', 'pointerleave', 'pointercancel']) btn.addEventListener(ev, () => clearTimeout(timer))
  btn.addEventListener('contextmenu', (e) => e.preventDefault())
  btn.addEventListener('click', () => { if (held) { held = false; return } close(); submit() })
  menu.addEventListener('keydown', (e) => { if (e.key === 'Escape') { close(); ta.focus() } })
  const wrap = h('div', { class: 'send-wrap' }, menu, btn)
  return wrap
}

// ---- Status line: what the agent's CLI last said of its model and
// spending, as Claude Code's own status line shows it.

// branches are the agents' workspace branches, as last read, and when.
const branches = new Map()

function readBranch(a) {
  const st = a.state?.status || 'idle'
  const b = branches.get(a.id)
  // Read again once a turn ends, which may have switched it, or a minute on.
  if (b && (b.pending || b.status === st || st === 'working') && Date.now() - b.at < 60000) return b.branch
  branches.set(a.id, { ...b, pending: true, status: st, at: Date.now() })
  api('GET', `/v1/agents/${a.id}/branch`)
    .then((r) => { branches.set(a.id, { branch: r.branch, status: st, at: Date.now() }); renderStatusLine() })
    .catch(() => branches.set(a.id, { ...branches.get(a.id), pending: false }))
  return b?.branch || ''
}

// modelName is claude-opus-5-5 as Opus 5.5; any other model as it is.
function modelName(id) {
  const m = /^claude-([a-z]+)-(\d+)-(\d+)/.exec(id || '')
  return m ? `${m[1][0].toUpperCase()}${m[1].slice(1)} ${m[2]}.${m[3]}` : id || ''
}

function tokens(n) {
  if (n >= 1e6) return +(n / 1e6).toFixed(n % 1e6 ? 1 : 0) + 'M'
  if (n >= 1e3) return Math.round(n / 1e3) + 'k'
  return String(n)
}

// left is the time until a unix second, in its two largest units.
function left(at) {
  const s = Math.max(0, at - Date.now() / 1000)
  const d = Math.floor(s / 86400), hr = Math.floor(s % 86400 / 3600), m = Math.floor(s % 3600 / 60)
  return d ? `${d}d${hr}h` : hr ? `${hr}h${m}m` : `${m}m`
}

const windowNames = { five_hour: '5h', seven_day: '7d' }

// planLimits are a's plan windows as its isolation's agents of its kind
// last said them: they are its CLI's account's, not its own, and an agent
// that has not run since a window started over says the one before.
function planLimits(a) {
  const latest = new Map()
  for (const b of S.agents) {
    if (b.archived_at || b.isolation !== a.isolation || b.kind !== a.kind) continue
    for (const l of b.state?.usage?.limits || []) {
      // Within one window use only grows: the most said is the latest.
      const k = latest.get(l.window), at = l.resets_at || 0, kat = k?.resets_at || 0
      if (!k || kat < at || kat === at && k.percent < l.percent) latest.set(l.window, l)
    }
  }
  const now = Date.now() / 1000
  return [...latest.values()].filter((l) => !l.resets_at || l.resets_at > now).sort((p, q) => p.window.localeCompare(q.window))
}

// liveTasks are what the agent's CLI runs beside its turn: subagents and
// shells run in the background.
const liveTasks = (a) => a.archived_at ? [] : a.state?.tasks || []
const taskIcon = (t) => t.kind === 'agent' ? 'bot' : t.kind === 'shell' ? 'terminal' : 'clock'
// taskMarks are a kind's icon, with how many when counted: subagents, then
// shells, then any other kind.
function taskMarks(tasks, counted) {
  const kinds = [...new Set(tasks.map((t) => t.kind))].sort((x, y) => (x === 'agent' ? 0 : x === 'shell' ? 1 : 2) - (y === 'agent' ? 0 : y === 'shell' ? 1 : 2))
  return kinds.map((k) => h('span', { class: 'task-mark' }, icon(taskIcon({ kind: k })),
    counted ? String(tasks.filter((t) => t.kind === k).length) : null))
}
const taskKindName = (t) => t.kind === 'agent' ? 'subagent' + (t.subagent ? ' · ' + t.subagent : '') : t.kind
function tasksSummary(tasks) {
  const n = (k, one) => { const c = tasks.filter((t) => t.kind === k).length; return c ? plural(c, one) : null }
  const other = tasks.filter((t) => t.kind !== 'agent' && t.kind !== 'shell').length
  return [n('agent', 'subagent'), n('shell', 'shell'), other ? plural(other, 'other task') : null].filter(Boolean).join(', ') + ' running'
}

// renderTasks is the list the status line's tasks item opens, over the
// message box.
function renderTasks() {
  const slot = document.getElementById('tasks-slot')
  if (!slot) return
  const a = agentById(S.sel)
  const tasks = a ? liveTasks(a) : []
  if (S.tasksOpen !== S.sel || !tasks.length) { S.tasksOpen = null; slot.replaceChildren(); return }
  slot.replaceChildren(h('div', { class: 'popover tasks-pop', role: 'dialog', 'aria-label': 'Running in the background' },
    h('h3', null, 'Running in the background'),
    h('div', { class: 'tasks' }, tasks.map((t) => h('div', { class: 'task' }, icon(taskIcon(t)),
      h('span', { class: 'task-what' }, t.description || '(no description)'),
      h('span', { class: 'task-kind' }, taskKindName(t)))))))
}

function closeTasks() {
  if (!S.tasksOpen) return false
  S.tasksOpen = null
  renderTasks()
  renderStatusLine()
  return true
}

function renderStatusLine() {
  const slot = document.getElementById('status-slot')
  const a = agentById(S.sel)
  if (!slot || !a) return
  const u = a.state?.usage || {}
  const tasks = liveTasks(a)
  renderTasks()
  if ((!a.state?.usage && !tasks.length) || a.archived_at) { slot.replaceChildren(); return }
  const dir = (u.cwd || a.workdir || '').replace(/^\/home\/agent(?=\/|$)/, '~')
  const branch = readBranch(a)
  // What runs beside the turn leads, so a wrapped line keeps it in view.
  const open = S.tasksOpen === a.id
  const parts = [
    tasks.length && h('span', null, h('button', {
      type: 'button', class: 'sl-tasks', 'aria-expanded': String(open), 'aria-label': tasksSummary(tasks), title: tasksSummary(tasks),
      onclick: (e) => { e.stopPropagation(); S.tasksOpen = open ? null : a.id; renderStatusLine() },
    }, ...taskMarks(tasks, true))),
    branch && h('span', { class: 'sl-branch' }, `(${branch})`),
    u.model && h('span', { class: 'sl-model' }, modelName(u.model)),
  ]
  if (u.context) parts.push(h('span', null, `cache ${Math.floor(100 * (u.cached || 0) / u.context)}%`))
  if (u.window) {
    const used = Math.round(100 * (u.context || 0) / u.window)
    parts.push(h('span', { class: used >= 80 ? 'sl-low' : used >= 50 ? 'sl-warn' : null },
      `ctx ${used}% (${tokens(u.context || 0)}/${tokens(u.window)})`))
  }
  for (const l of planLimits(a)) {
    const name = windowNames[l.window]
    if (name) parts.push(h('span', { class: l.percent >= 90 ? 'sl-low' : l.percent >= 70 ? 'sl-warn' : null },
      `${name} ${Math.round(l.percent)}%` + (l.resets_at ? ` (${left(l.resets_at)})` : '')))
  }
  if (u.cost_usd) parts.push(h('span', null, '$' + u.cost_usd.toFixed(2)))
  const shown = parts.filter(Boolean)
  // The directory has a line of its own: it is often too long to share one.
  slot.replaceChildren(...[
    // The marks keep the path in its order in a box set right to left.
    dir && h('div', { class: 'sl-dir', title: dir }, '\u200e' + dir + '\u200e'),
    shown.length && h('div', { class: 'sl-row' }, h('div', { class: 'sl-items' }, ...shown)),
  ].filter(Boolean))
}

function renderSettings(a) {
  const slot = document.getElementById('settings-slot')
  if (!slot) return
  document.querySelector('.composer .icon-btn')?.classList.toggle('on', S.settingsOpen)
  if (!S.settingsOpen) { slot.replaceChildren(); return }
  const o = a.options || {}
  const effort = dropdown(effortOptions(a.kind), { value: o.effort || '', label: 'Effort' })
  const model = h('input', { value: o.model || '', placeholder: "the CLI's default" })
  const approvals = dropdown([
    { value: 'never', label: 'never — run unattended' },
    { value: 'auto', label: 'auto — a reviewer model refuses risky actions' },
    { value: 'ask', label: 'ask — the agent asks first' },
    { value: 'default', label: "default — the CLI's own settings decide" },
  ], { value: o.approvals || 'never', label: 'Approvals' })
  slot.replaceChildren(h('div', { class: 'popover' },
    h('h3', null, 'Agent settings'),
    h('div', { class: 'field' }, h('label', null, 'Effort'), effort),
    h('div', { class: 'field' }, h('label', null, 'Model'), model),
    h('div', { class: 'field' }, h('label', null, 'Approvals'), approvals),
    h('div', { class: 'note' }, a.kind === 'claude' ? 'Claude restarts with these when it is idle, with nothing in the background.' : 'Codex uses these from the next turn.'),
    h('div', { class: 'actions popover-actions' },
      h('button', {
        class: 'btn primary', onclick: async () => {
          const opts = { effort: effort.value || undefined, model: model.value.trim() || undefined, approvals: approvals.value }
          await guarded(async () => {
            await api('PUT', `/v1/agents/${a.id}/options`, opts)
            S.settingsOpen = false
            renderSettings(a)
            toast('Settings saved')
            await refreshAgents()
          })
        },
      }, 'Save'),
      h('button', { class: 'btn', onclick: () => { S.settingsOpen = false; renderSettings(a) } }, 'Cancel'))))
}

// dirCompleter offers the isolation's directories that what is typed in
// input could go on to, asked of the isolation as typing pauses: arrows
// choose, Tab or Enter takes one and goes on into it, Esc closes the list.
// A path typed with ~ keeps it.
// The working directory last given for a new agent, per isolation. It
// is a convenience of this browser only.
function recentDir(iso) {
  try { return JSON.parse(localStorage.getItem('psps.recentDirs') || '{}')[iso] || null } catch { return null }
}

function rememberDir(iso, dir) {
  try {
    const all = JSON.parse(localStorage.getItem('psps.recentDirs') || '{}')
    all[iso] = dir
    localStorage.setItem('psps.recentDirs', JSON.stringify(all))
  } catch {}
}

// o, for directories elsewhere than an isolation (the Mac's, for the
// admin agent): list(typed) answers as /dirs does, recents() are the
// directories offered first, recentTitle says what they are, missing is
// said when nothing is there, onAccept hears of each one taken.
function dirCompleter(input, isoName, o = {}) {
  const box = h('div', { class: 'complete below', role: 'listbox', hidden: true })
  let items = [], active = 0, seq = 0, timer = null
  const close = () => { box.hidden = true; items = []; seq++ }
  let recent = new Set()
  const list = o.list || ((typed) => api('GET', `/v1/isolations/${encodeURIComponent(isoName())}/dirs?path=${encodeURIComponent(typed)}`))
  const recents = o.recents || (() => { const r = recentDir(isoName()); return r ? [r] : [] })
  const draw = (note) => {
    box.replaceChildren(...items.map((d, i) => h('div', {
      class: 'complete-item dir' + (i === active ? ' active' : ''), role: 'option', 'aria-selected': String(i === active),
      onmousedown: (e) => { e.preventDefault(); active = i; accept() },
    }, h('span', { class: 'ci-name' }, d),
      recent.has(d) ? h('span', { class: 'ci-tag', title: o.recentTitle || 'The directory last given for an agent in this isolation' }, 'recent') : null)),
    ...(note ? [h('div', { class: 'complete-note' }, note)] : []))
    box.hidden = !items.length && !note
    box.querySelector('.active')?.scrollIntoView({ block: 'nearest' })
  }
  const ask = async () => {
    if ((!o.list && !isoName()) || document.activeElement !== input) return close()
    const my = ++seq
    const typed = input.value
    try {
      const d = await list(typed)
      if (my !== seq) return
      const tilde = (p) => (typed.trim() === '' || typed.trim().startsWith('~')) && (p + '/').startsWith(d.home + '/') ? '~' + p.slice(d.home.length) : p
      items = d.dirs.map(tilde)
      // The ones last given lead, while what is typed could still become
      // them.
      const t = typed.trim()
      const lead = recents().filter((r) => r !== t && r !== t.replace(/\/$/, '') && r.startsWith(t))
      recent = new Set(lead)
      items = [...lead, ...items.filter((p) => !recent.has(p) && !recent.has(p.replace(/\/$/, '')))]
      active = 0
      draw(items.length ? null : (o.missing || 'No such directory here: it is made when the agent is.'))
    } catch (e) {
      if (my !== seq) return
      items = []
      draw(e instanceof Unauthorized ? null : `Can't list it: ${e.message}`)
    }
  }
  const later = () => { clearTimeout(timer); timer = setTimeout(ask, 120) }
  const accept = () => {
    if (!items[active]) return false
    input.value = items[active]
    input.setSelectionRange(input.value.length, input.value.length)
    o.onAccept?.()
    later()
    return true
  }
  input.addEventListener('input', later)
  input.addEventListener('focus', later)
  input.addEventListener('blur', close)
  input.addEventListener('keydown', (e) => {
    if (box.hidden || e.isComposing) return
    switch (e.key) {
      case 'ArrowDown': case 'ArrowUp':
        if (!items.length) return
        e.preventDefault()
        active = (active + (e.key === 'ArrowDown' ? 1 : items.length - 1)) % items.length
        draw()
        return
      case 'Tab': case 'Enter':
        if (items.length && !e.shiftKey) { e.preventDefault(); accept() }
        return
      case 'Escape':
        e.preventDefault()
        e.stopPropagation()
        close()
    }
  })
  input.addEventListener('input', () => o.onAccept?.())
  return h('div', { class: 'with-complete' }, input, box)
}

// The isolation, agent and working directory the last new agent was made
// with, offered for the next. A convenience of this browser only.
function lastNewAgent() {
  try { return JSON.parse(localStorage.getItem('psps.lastNewAgent') || '{}') || {} } catch { return {} }
}

function rememberNewAgent(last) {
  try { localStorage.setItem('psps.lastNewAgent', JSON.stringify(last)) } catch {}
}

function newAgentView() {
  const last = lastNewAgent()
  let kind = last.kind === 'codex' ? 'codex' : 'claude'
  const err = h('p', { class: 'form-error' })
  const iso = dropdown([], { label: 'Isolation' })
  // The last one is chosen once it is in the list, unless another was
  // chosen first.
  let wantIso = last.isolation || null
  iso.addEventListener('change', () => { wantIso = null })
  const noIso = h('p', { class: 'form-error' }, 'There is no isolation yet. ',
    navLink('#/isolations/' + NEW, null, 'Create one'), ' first.')
  // The list may arrive after the form is drawn; refill it in place so
  // what is already chosen survives.
  const fillIsos = () => {
    iso.setOptions(S.isolations.map((i) => i.name))
    if (wantIso && S.isolations.some((i) => i.name === wantIso)) { iso.value = wantIso; wantIso = null }
    noIso.hidden = S.isolations.length > 0
  }
  fillIsos()
  S.refillIsolations = fillIsos
  const handle = h('input', { id: 'new-handle', placeholder: 'optional; like brave-otter otherwise', autocomplete: 'off', autocapitalize: 'off', spellcheck: 'false' })
  const [team, teams] = teamInput('')
  team.className = ''
  team.placeholder = 'optional; in none otherwise'
  const workdir = h('input', { placeholder: 'optional; ~/work/<its ID> otherwise', autocomplete: 'off', autocapitalize: 'off', spellcheck: 'false' })
  workdir.value = typeof last.workdir === 'string' ? last.workdir : ''
  const effort = dropdown([], { label: 'Effort' })
  const model = h('input', { placeholder: "optional; the CLI's default otherwise" })
  const approvals = dropdown([
    { value: 'never', label: 'never — run unattended inside the isolation' },
    { value: 'auto', label: 'auto — unattended; a reviewer model refuses risky actions' },
    { value: 'ask', label: 'ask — the agent asks first for what its own rules flag' },
    { value: 'default', label: "default — the CLI's own settings in the isolation decide" },
  ], { value: 'default', label: 'Approvals' })
  const fillEfforts = () => effort.setOptions(effortOptions(kind))
  fillEfforts()
  const seg = h('div', { class: 'segmented' })
  const drawSeg = () => seg.replaceChildren(...['claude', 'codex'].map((k) =>
    h('button', { type: 'button', class: k === kind ? 'on' : '', onclick: () => { kind = k; drawSeg(); fillEfforts() } }, k === 'codex' ? 'Codex' : 'Claude')))
  drawSeg()
  const prompt = h('textarea', {
    id: 'new-prompt',
    placeholder: 'optional; it waits for your first message otherwise',
    title: coarse ? null : 'Enter creates · Shift+Enter adds a line',
  })
  const create = h('button', { class: 'btn primary' })
  const idle = () => { create.textContent = prompt.value.trim() ? 'Create and send' : 'Create agent' }
  idle()
  const submit = async () => {
    err.textContent = ''
    if (!iso.value) { err.textContent = 'Create an isolation first.'; return }
    create.disabled = true
    create.textContent = 'Creating…'
    try {
      const a = await api('POST', '/v1/agents', {
        isolation: iso.value, kind, handle: handle.value.trim() || undefined, prompt: prompt.value.trim() || undefined,
        workdir: workdir.value.trim() || undefined, team: team.value.trim() || undefined,
        options: { effort: effort.value || undefined, model: model.value.trim() || undefined, approvals: approvals.value },
      })
      if (workdir.value.trim()) rememberDir(iso.value, workdir.value.trim())
      rememberNewAgent({ isolation: iso.value, kind, workdir: workdir.value.trim() })
      await refreshAgents()
      select(a.id)
      // Without a first message it waits for one.
      document.querySelector('.composer textarea')?.focus()
    } catch (e) {
      if (e instanceof Unauthorized) { S.authed = false; render(); return }
      err.textContent = e.message
      create.disabled = false
      idle()
    }
  }
  create.addEventListener('click', submit)
  prompt.addEventListener('input', () => { if (!create.disabled) idle() })
  prompt.addEventListener('keydown', (e) => {
    if (e.key === 'Enter' && !e.shiftKey && !coarse && !e.isComposing) { e.preventDefault(); if (!create.disabled) submit() }
  })
  return h('div', { class: 'page' }, h('div', { class: 'page-inner' },
    h('h2', null, 'New agent'),
    h('p', { class: 'lead' }, 'It runs inside an isolation, in a workspace of its own, and starts on its first message: written here, or sent once it is made.'),
    noIso,
    h('div', { class: 'row' },
      h('div', { class: 'field' }, h('label', null, 'Isolation'), iso),
      h('div', { class: 'field' }, h('label', null, 'Agent'), seg)),
    h('div', { class: 'field' }, h('label', null, 'ID'), handle,
      h('span', { class: 'hint' }, 'Lowercase letters, digits and dashes; unique among all agents, archived ones too. Left empty, it gets an adjective-noun no agent has.')),
    h('div', { class: 'field' }, h('label', null, 'Team'), team, teams,
      h('span', { class: 'hint' }, 'The sidebar groups a team\'s agents, and they move within it. A name no team has makes one.')),
    h('div', { class: 'field' }, h('label', null, 'Working directory'), dirCompleter(workdir, () => iso.value),
      h('span', { class: 'hint' }, 'Where in the isolation it works, like ~/work/trevia; made if it is not there. ~ is the agent\'s home. Left empty, it gets a workspace of its own.')),
    h('div', { class: 'row' },
      h('div', { class: 'field' }, h('label', null, 'Effort'), effort),
      h('div', { class: 'field' }, h('label', null, 'Model'), model)),
    h('div', { class: 'field' }, h('label', null, 'Approvals'), approvals),
    h('div', { class: 'field' }, h('label', null, 'First message'), prompt,
      h('span', { class: 'hint' }, 'Sent as soon as the agent is made, and names it, as a first message does.')),
    err,
    create))
}

// ---- The settings pages (isolations, services, accounts): a list, and
// one of it in detail. Side by side where the page is wide; where it is
// narrow (a phone, a narrow window) the list is the page and an item
// replaces it, with the way back on top. The address names the item.

// split fills page with the list and the detail. chosen: the address names
// the item the detail shows, which a narrow page shows instead of the list.
function split(page, chosen, label, master, detail) {
  // Polling redraws the page: a list scrolled on its own stays where it was.
  const scrolled = page.querySelector(':scope > .master')?.scrollTop || 0
  page.classList.toggle('chosen', !!chosen)
  page.replaceChildren(
    h('nav', { class: 'master', 'aria-label': label }, master),
    h('section', { class: 'detail' }, detail))
  if (scrolled) page.querySelector(':scope > .master').scrollTop = scrolled
}

// navLink is a link within the page: drawn by go(), opened anew by the
// browser's own ways (a new tab).
function navLink(href, cls, ...kids) {
  return h('a', {
    class: cls || null, href,
    onclick: (e) => { if (e.button || e.metaKey || e.ctrlKey || e.shiftKey || e.altKey) return; e.preventDefault(); go(href) },
  }, kids)
}

function masterItem(href, on, ...kids) {
  const a = navLink(href, 'master-item' + (on ? ' on' : ''), kids)
  if (on) a.setAttribute('aria-current', 'page')
  return a
}

const backLink = (href, text) => navLink(href, 'back', icon('chevron-left'), text)
const cap = (s) => s ? s[0].toUpperCase() + s.slice(1) : s
const plural = (n, one) => `${n} ${one}${n === 1 ? '' : 's'}`

// block is a titled part of a detail.
function block(title, ...body) {
  return h('section', { class: 'block' }, title ? h('h3', null, title) : null, body)
}

// kv is labels and their values; a pair left out (null) is not drawn.
function kv(pairs) {
  return h('dl', { class: 'kv' }, pairs.filter(Boolean).map(([k, v]) => [h('dt', null, k), h('dd', null, v)]))
}

// ---- Isolations

// isoUp: running and able to work; isoState says which, or what is wrong.
const isoUp = (i) => i.state === 'running' && !i.health
const isoState = (i) => cap(i.health || i.state)
const fenceOK = (i) => i.fence_state === 'verified' || i.fence_state === 'off'
const fenceText = (i) => i.fence_state === 'verified' ? 'fence verified' : i.fence_state === 'off' ? 'fence off' : 'fence ' + i.fence_state
const isoAgents = (i) => S.agents.filter((a) => !a.archived_at && a.isolation === i.name)
const isoTabNames = { overview: 'Overview', services: 'Services', logins: 'Logins', environment: 'Environment', settings: 'Settings' }

// shownIso is the isolation the detail shows: the address's, or the first.
function shownIso() {
  if (S.isoSel === NEW) return null
  return S.isolations.find((i) => i.name === S.isoSel) || S.isolations[0] || null
}

function renderIsolations() {
  const page = document.getElementById('isolations')
  if (!page) return
  const making = S.isoSel === NEW || !S.isolations.length
  const shown = making ? null : shownIso()
  const chosen = S.isoSel === NEW || (!!S.isoSel && shown?.name === S.isoSel)
  split(page, chosen, 'Isolations', [
    h('div', { class: 'master-head' },
      h('span', { class: 'label' }, plural(S.isolations.length, 'isolation')),
      navLink('#/isolations/' + NEW, 'btn small', '+ New')),
    S.isolations.map((i) => isoItem(i, i === shown)),
  ], making ? newIsolationView() : isoDetail(shown))
}

function isoItem(i, on) {
  const n = isoAgents(i).length
  const xs = (S.exposures[i.name] || []).filter((x) => x.enabled).map((x) => x.service)
  return masterItem(isoHash(i.name), on,
    h('span', { class: 'item-line' },
      h('span', { class: 'dot' + (isoUp(i) ? ' on' : '') }),
      h('strong', null, i.name),
      h('span', { class: 'item-side' }, n ? plural(n, 'agent') : 'no agents')),
    h('span', { class: 'item-sub' + (fenceOK(i) && !i.health ? '' : ' warn') }, `${isoState(i)} · ${fenceText(i)}`),
    xs.length ? h('span', { class: 'item-sub' }, xs.join(', ')) : null)
}

function isoDetail(i) {
  const on = (S.exposures[i.name] || []).filter((x) => x.enabled).length
  const tab = S.isoTab
  return [
    backLink('#/isolations', 'Isolations'),
    h('div', { class: 'detail-head' },
      h('h2', null, i.name),
      h('span', { class: 'chip ' + (isoUp(i) ? 'ok' : 'blocked') }, isoState(i)),
      h('span', { class: 'chip ' + (i.fence_state === 'verified' ? 'ok' : i.fence_state === 'off' ? '' : 'blocked') }, cap(fenceText(i))),
      h('span', { class: 'grow' }),
      h('span', { class: 'head-actions' },
        h('button', { class: 'btn', disabled: S.verifying === i.name, onclick: () => verifyIsolation(i) },
          S.verifying === i.name ? 'Measuring…' : 'Verify'),
        i.state === 'running'
          ? h('button', { class: 'btn danger', title: 'End its VM now, past container: for one that no longer answers', onclick: () => killIsolation(i) }, 'Kill')
          : h('button', { class: 'btn', disabled: S.starting === i.name, title: 'Start its container, its shim answering and its fence measured', onclick: () => startIsolation(i) },
            S.starting === i.name ? 'Starting…' : 'Start'),
        h('button', { class: 'btn', onclick: () => upgradeIsolation(i) }, 'Upgrade'))),
    i.health ? h('p', { class: 'callout warn' }, i.health === 'memory full'
      ? `It runs but has no memory left to work in: ${i.health_detail}. Its agents stall until that frees up; Kill restarts it.`
      : `It runs but does not answer: ${i.health_detail}. Kill restarts it.`) : null,
    fenceOK(i) ? null : h('p', { class: 'callout warn' }, `Agents start here only behind a verified network fence: ${i.fence_state}.`),
    S.openForm === `verify:${i.name}` && S.verifyOut ? verifyView(S.verifyOut) : null,
    h('div', { class: 'tabs', role: 'tablist' }, ISO_TABS.map((t) => {
      const a = navLink(isoHash(i.name, t), 'tab' + (t === tab ? ' on' : ''), isoTabNames[t],
        t === 'services' ? h('span', { class: 'count' }, String(on)) : null)
      a.setAttribute('role', 'tab')
      a.setAttribute('aria-selected', String(t === tab))
      return a
    })),
    h('div', { class: 'tab-body', role: 'tabpanel' },
      { overview: isoOverview, services: isoServices, logins: isoLogins, environment: isoEnvironment, settings: isoSettings }[tab](i)),
  ]
}

function isoOverview(i) {
  const agents = isoAgents(i)
  const login = (k) => i.accounts?.[k] ? `Account ${i.accounts[k]}` : 'A login inside the isolation'
  const xs = (S.exposures[i.name] || []).filter((x) => x.enabled).map((x) => x.service)
  return [
    block('Machine', kv([
      ['State', i.health ? `${isoState(i)}: ${i.health_detail}` : cap(i.state)],
      ['Network fence', i.fence_state === 'verified' ? 'Verified' : i.fence_state === 'off' ? 'Off: it reaches the Mac and the LAN' : cap(i.fence_state)],
      i.proxies && i.proxies !== 'up' ? ['Proxies', i.proxies] : null,
      ['Image', h('code', null, i.image)],
      ['CPUs, memory', isoSize(i.cpus, i.memory, i.swap)],
      ['Network', h('code', null, i.network || 'default')],
    ])),
    block(`Agents · ${agents.length}`, agents.length
      ? h('div', { class: 'list' }, agents.map((a) => h('a', {
        class: 'list-row link', href: '#/a/' + a.id,
        onclick: (e) => { if (e.button || e.metaKey || e.ctrlKey) return; e.preventDefault(); select(a.id) },
      },
      h('span', { class: 'dot ' + agentState(a) }),
      h('span', { class: 'list-main' }, h('strong', null, agentTitle(a)), h('span', { class: 'list-sub' }, `${shownID(a)} · ${label(a)}`)),
      icon('chevron-right'))))
      : h('p', { class: 'note' }, 'None live.')),
    block('Logins', kv([['Claude Code', login('claude')], ['Codex', login('codex')]]),
      navLink(isoHash(i.name, 'logins'), 'more-link', 'Logins ', icon('chevron-right'))),
    block('Services', h('p', null, xs.length ? xs.join(', ') : 'None on.'),
      navLink(isoHash(i.name, 'services'), 'more-link', 'Services ', icon('chevron-right'))),
  ]
}

// The services, grouped by what they give the agents.
const SERVICE_GROUPS = [
  { name: 'Credentials', note: 'your logins, used on the Mac within an allowlist' },
  { name: 'Into the isolation', note: 'folders, skills and ports of the Mac’s' },
  { name: 'Between agents', note: 'psps’s own' },
]
const serviceGroup = (s) => ['share', 'skill', 'port'].includes(s.name) ? 1 : ['mail', 'cron', 'team', 'control', 'job'].includes(s.name) ? 2 : 0
const grouped = (services) => SERVICE_GROUPS.map((g, n) => ({ ...g, services: services.filter((s) => serviceGroup(s) === n) })).filter((g) => g.services.length)

function isoServices(i) {
  const xs = S.exposures[i.name]
  if (!xs || !S.services.length) return h('p', { class: 'note' }, 'Loading…')
  const byName = Object.fromEntries(xs.map((x) => [x.service, x]))
  return [
    h('p', { class: 'note' }, 'The credentials stay on the Mac; the isolation gets a proxy that uses them within an allowlist.'),
    grouped(S.services).map((g) => h('div', { class: 'group' },
      h('div', { class: 'group-head' }, h('strong', null, g.name), h('span', { class: 'note' }, g.note)),
      h('div', { class: 'list' }, g.services.map((s) => serviceToggle(i, s, byName[s.name] || { service: s.name, enabled: false, envs: [] }))))),
  ]
}

// serviceToggle is a service in an isolation: its switch, and what it
// serves there, which opens the form that changes it.
function serviceToggle(i, s, x) {
  const key = `expose:${i.name}:${s.name}`
  const open = S.openForm === key
  return h('div', { class: 'svc-toggle' + (x.enabled ? ' on' : '') },
    h('div', { class: 'toggle-line' },
      h('button', {
        class: 'switch' + (x.enabled ? ' on' : ''), role: 'switch', 'aria-checked': String(x.enabled),
        'aria-label': `${s.name} in ${i.name}`, onclick: () => flipService(i, s, x, key),
      }),
      h('button', { class: 'toggle-main', 'aria-expanded': String(open), onclick: () => open ? closeForm() : openForm(key) },
        h('span', { class: 'toggle-name' }, s.name),
        h('span', { class: 'toggle-sum' }, exposureText(s, x))),
      x.enabled && s.forwards && x.forwards?.length ? h('span', { class: 'toggle-links' }, forwardLinks(x.forwards)) : null,
      x.missing?.length ? h('span', { class: 'chip error' }, 'missing ' + x.missing.join(',')) : null,
      (x.forwards || []).filter((f) => f.error).map((f) => h('span', { class: 'chip error', title: f.error }, `${f.env} not listening`))),
    open ? exposeForm(i, s, x) : null)
}

// exposureText is what a service serves in an isolation, or, off, what
// turning it on takes.
function exposureText(s, x) {
  const envs = S.envs.filter((e) => e.service === s.name)
  if (!x.enabled) {
    if (!s.fields?.length) return s.summary
    if (!envs.length) return 'No environment yet'
    return envs.length === 1 ? `${envs[0].name} is ready to turn on` : `${envs.length} environments are ready to turn on`
  }
  if (s.replaces) return (x.envs || []).map((n) => '/shared/' + n).join(', ') || 'no folder'
  if (s.forwards) return x.forwards?.length ? '' : 'no port'
  const parts = []
  if (s.fields?.length) parts.push((x.envs || []).join(', ') || 'no environment')
  if (x.default_env && !s.one_env && (x.envs || []).length > 1) parts.push(`default ${x.default_env}`)
  parts.push(...settingsText(s.settings, x.settings))
  return parts.join(' · ') || 'on'
}

// settingsText is an exposure's settings in words: a flag by its name, a
// list with its entries.
function settingsText(fields, cfg) {
  return (fields || []).flatMap((f) => {
    const v = cfg?.[f.key]
    const name = f.key.replaceAll('_', ' ')
    if (v === true) return [name]
    if (v === false) return ['no ' + name]
    if (Array.isArray(v)) return v.length ? [`${name} ${v.join(', ')}`] : []
    return v != null && v !== '' ? [`${name} ${v}`] : []
  })
}

// flipService is a service's switch: on, it turns it off (asking); off, it
// turns on at once what has nothing to choose, and opens the form of the
// rest.
async function flipService(i, s, x, key) {
  if (x.enabled) return turnOff(i, s)
  if (s.fields?.length) return openForm(key)
  await guarded(async () => {
    await api('PUT', `/v1/isolations/${enc(i.name)}/services/${enc(s.name)}`, { envs: [], settings: { set: {}, clear: [] } })
    toast(`${s.name} is on for ${i.name}; agents pick it up at their next idle moment`)
    await refreshIsolations()
  })
}

async function turnOff(i, s) {
  if (!await ask({
    title: `Turn ${s.name} off for ${i.name}?`,
    body: s.replaces ? 'Its container is replaced without the shared folders: running agents end and start again on their next message.'
      : s.forwards ? 'The Mac stops listening for its ports, and connections through them end.'
        : 'Its files and variables are removed there.',
    ok: 'Turn off', danger: true,
  })) return
  await guarded(async () => {
    await api('DELETE', `/v1/isolations/${enc(i.name)}/services/${enc(s.name)}`)
    S.openForm = null
    toast(`${s.name} is off for ${i.name}`)
    await refreshIsolations()
    renderIsolations()
  })
}

function isoLogins(i) {
  return [
    h('p', { class: 'note' }, 'What Claude Code and Codex log in with here. The shared accounts are on ',
      navLink(setupHash('iso', i.name), null, 'Accounts'), '.'),
    cliCards(i),
  ]
}

function cliCards(i) {
  const cs = S.clis[i.name]
  if (cs == null) return h('p', { class: 'note' }, 'Reading the logins from inside the isolation…')
  if (!Array.isArray(cs)) return h('p', { class: 'form-error' }, cs.error)
  return h('div', { class: 'cli-cards' }, cs.map((c) => cliRow(i, c)))
}

function isoEnvironment(i) {
  const keys = i.env_keys || []
  const key = `envvar:${i.name}`
  return [
    h('p', { class: 'note' }, 'Variables every agent in the isolation starts with. Their values are never shown; idle agents restart with a change.'),
    keys.length ? h('div', { class: 'list' }, keys.map((k) => h('div', { class: 'list-row' },
      h('code', { class: 'list-main' }, k),
      h('button', {
        class: 'btn ghost', onclick: async () => {
          if (!await ask({ title: `Remove ${k} from ${i.name}?`, body: 'Idle agents restart without it.', ok: 'Remove', danger: true })) return
          await setIsoEnv(i, k, null)
        },
      }, 'Remove'))))
      : h('p', { class: 'note' }, 'None set.'),
    S.openForm === key ? envVarForm(i) : h('div', { class: 'actions' }, h('button', { class: 'btn', onclick: () => openForm(key) }, '+ Add variable')),
  ]
}

function envVarForm(i) {
  const name = h('input', { placeholder: 'VARIABLE', autocomplete: 'off' })
  const value = h('input', { type: 'password', autocomplete: 'new-password' })
  const set = h('button', { class: 'btn primary' }, 'Set')
  set.addEventListener('click', async () => {
    const k = name.value.trim()
    if (!k) return
    if (await setIsoEnv(i, k, value.value)) closeForm()
  })
  return h('div', { class: 'subform' },
    h('div', { class: 'row' },
      h('div', { class: 'field' }, h('label', null, 'Variable'), name),
      h('div', { class: 'field' }, h('label', null, 'Value'), value, h('span', { class: 'hint' }, 'Setting one that is there replaces its value.'))),
    h('div', { class: 'actions' }, set, h('button', { class: 'btn', onclick: closeForm }, 'Cancel')))
}

async function setIsoEnv(i, k, v) {
  let ok = false
  await guarded(async () => {
    await api('PATCH', `/v1/isolations/${enc(i.name)}/env`, { [k]: v === '' ? null : v })
    toast(v == null || v === '' ? `${k} removed; idle agents restart without it` : `${k} saved; idle agents restart with it`)
    ok = true
    await refreshIsolations()
  })
  return ok
}

function isoSettings(i) {
  const key = `settings:${i.name}`
  return [
    block('Name and size', S.openForm === key ? isoSettingsForm(i) : [
      kv([['Name', i.name], ['CPUs', i.cpus ? String(i.cpus) : 'container’s default'], ['Memory', i.memory || 'container’s default'], ['Swap', i.swap || 'none']]),
      h('div', { class: 'actions' }, h('button', { class: 'btn', onclick: () => openForm(key) }, 'Edit')),
    ]),
    block('Delete', h('p', { class: 'note' }, 'The isolation and its container go; its volumes are kept.'),
      h('div', { class: 'actions' }, h('button', { class: 'btn danger', onclick: () => deleteIsolation(i) }, 'Delete isolation'))),
  ]
}

async function upgradeIsolation(i) {
  if (!await ask({ title: `Upgrade ${i.name} to the current image?`, body: 'Running agents restart; sessions are kept.', ok: 'Upgrade' })) return
  toast('Upgrading…')
  await guarded(async () => { await api('POST', `/v1/isolations/${enc(i.name)}/upgrade`); toast(`${i.name} upgraded`); await refreshIsolations() })
}

// startIsolation brings a stopped one up: one with no agents stays down
// after a kill until something needs it.
async function startIsolation(i) {
  S.starting = i.name
  rerender()
  try {
    await guarded(async () => {
      await api('POST', `/v1/isolations/${enc(i.name)}/start`)
      toast(`${i.name} started`)
    })
    await refreshIsolations().catch(() => {})
  } finally {
    S.starting = null
    rerender()
  }
}

// killIsolation ends the container's VM with SIGKILL, past container: a
// VM too starved to start anything holds up every exec, container's stop
// with them, and only this ends it.
async function killIsolation(i) {
  if (!await ask({
    title: `Kill ${i.name}?`,
    body: 'Its VM ends at once, as if unplugged: its agents are interrupted, and what its disks had not written is lost. It starts again when something next needs it. For an isolation that no longer answers, when Stop would hang too.',
    ok: 'Kill', danger: true,
  })) return
  await guarded(async () => {
    const got = await api('POST', `/v1/isolations/${enc(i.name)}/kill`)
    toast(got.processes?.length ? `${i.name} killed` : `${i.name} was not running`)
    await refreshIsolations()
  })
}

async function deleteIsolation(i) {
  if (!await ask({ title: `Delete isolation ${i.name}?`, body: 'Its volumes are kept.', ok: 'Delete', danger: true })) return
  await guarded(async () => {
    await api('DELETE', `/v1/isolations/${enc(i.name)}`)
    toast(`${i.name} deleted`)
    go('#/isolations')
  })
}

// newIsolationView makes one. What is typed, and the one being created,
// live in S, since polling redraws this page: drawn from the DOM it came
// back empty two seconds after Create, as if nothing had happened.
function newIsolationView() {
  const d = S.isoDraft
  const busy = !!S.isoCreating
  const name = h('input', { placeholder: 'name', autocomplete: 'off', value: d.name, disabled: busy })
  const memory = h('input', { value: d.memory, disabled: busy })
  const fence = h('input', { type: 'checkbox', checked: d.fence, disabled: busy })
  name.addEventListener('input', () => { d.name = name.value })
  memory.addEventListener('input', () => { d.memory = memory.value })
  fence.addEventListener('change', () => { d.fence = fence.checked })
  const create = h('button', { class: 'btn primary', disabled: busy },
    busy ? `Creating ${S.isoCreating}… (building its image first when psps has changed)` : 'Create isolation')
  create.addEventListener('click', async () => {
    const n = d.name.trim()
    if (!n) return
    S.isoCreating = n
    S.isoCreateError = null
    renderIsolations()
    let made = false
    try {
      await api('POST', '/v1/isolations', { name: n, memory: d.memory.trim(), fence: d.fence ? 'on' : 'off' })
      S.isoDraft = { name: '', memory: '4G', fence: true }
      toast(`Isolation ${n} created`)
      made = true
    } catch (x) {
      if (x instanceof Unauthorized) { S.authed = false; render(); return }
      S.isoCreateError = x.message
      if (NO_KERNEL.test(x.message)) noKernel()
    } finally {
      S.isoCreating = null
    }
    await guarded(refreshIsolations)
    if (made && S.view === 'isolations') go(isoHash(n))
    else renderIsolations()
  })
  return [
    S.isolations.length ? backLink('#/isolations', 'Isolations') : null,
    h('div', { class: 'detail-head' }, h('h2', null, 'New isolation')),
    h('p', { class: 'note' }, 'Each runs in its own VM, fenced off the Mac and the LAN, with internet access.'),
    h('div', { class: 'row spaced' },
      h('div', { class: 'field' }, h('label', null, 'Name'), name),
      h('div', { class: 'field' }, h('label', null, 'Memory'), memory)),
    h('label', { class: 'check' }, fence, 'Network fence (agents start only behind a verified fence)'),
    S.isoCreateError ? h('p', { class: 'form-error' }, S.isoCreateError) : null,
    h('div', { class: 'actions' }, create),
  ]
}

// NO_KERNEL is container refusing to start a VM before a Linux kernel is
// set up, which a fresh install of container is: nothing in psps fixes it,
// so noKernel tells the operator the command that does.
const NO_KERNEL = /default kernel not configured/
function noKernel() {
  ask({
    title: 'container has no Linux kernel yet',
    body: 'Isolations run in a VM that needs one. Install the recommended kernel in a terminal on the Mac, then Create again:',
    extra: h('pre', { class: 'code dialog-cmd' }, h('code', null, 'container system kernel set --recommended')),
    cancel: 'Close',
  })
}

// isoSize is a VM's CPUs, memory and swap; none set is container's
// default, and no swap.
function isoSize(cpus, memory, swap) {
  return `${cpus ? cpus + ' CPUs' : 'default CPUs'}, ${memory || 'default memory'}${swap ? `, ${swap} swap` : ''}`
}

// isoSettingsForm renames an isolation, resizes its VM or sets its swap. A
// new name, or a later swap, goes straight through; a new size or the
// first swap replaces the container, so it asks first.
function isoSettingsForm(i) {
  const name = h('input', { value: i.name, autocomplete: 'off' })
  const cpus = h('input', { value: i.cpus ? String(i.cpus) : '', inputmode: 'numeric', placeholder: 'default', autocomplete: 'off' })
  const memory = h('input', { value: i.memory || '', placeholder: 'default', autocomplete: 'off' })
  const swap = h('input', { value: i.swap || '', placeholder: 'none', autocomplete: 'off' })
  const err = h('p', { class: 'form-error' })
  const save = h('button', { class: 'btn primary' }, 'Save')
  save.addEventListener('click', async () => {
    err.textContent = ''
    const body = {}
    const n = name.value.trim()
    if (!n) { err.textContent = 'A name is required.'; return }
    if (n !== i.name) body.name = n
    const c = cpus.value.trim() === '' ? 0 : Number(cpus.value.trim())
    if (!Number.isInteger(c)) { err.textContent = 'CPUs is a whole number, or empty for container’s default.'; return }
    if (c !== (i.cpus || 0)) body.cpus = c
    const m = memory.value.trim()
    if (m !== (i.memory || '')) body.memory = m
    const sw = swap.value.trim() === '0' ? '' : swap.value.trim()
    if (sw !== (i.swap || '')) body.swap = sw
    if (!Object.keys(body).length) { closeForm(); return }
    const resize = 'cpus' in body || 'memory' in body || (!i.swap && sw !== '')
    if (resize && !await ask({
      title: `Resize ${i.name} to ${isoSize(c, m, sw)}?`,
      body: 'The container is replaced: running agents restart. Sessions and workspaces are kept.',
      ok: 'Resize',
    })) return
    save.disabled = true
    save.textContent = resize ? 'Resizing…' : 'Saving…'
    try {
      const v = await api('PATCH', `/v1/isolations/${enc(i.name)}`, body)
      S.openForm = null
      // The page stays on it under its new name.
      if (v.name !== i.name && S.isoSel === i.name) {
        S.isoSel = v.name
        history.replaceState(null, '', isoHash(v.name, S.isoTab))
        S.routed = location.hash
      }
      toast(!resize ? (body.name ? `${i.name} is now ${v.name}; its agents kept running` : `${v.name} has ${isoSize(v.cpus, v.memory, v.swap)}; its agents kept running`)
        : body.name ? `${i.name} is now ${v.name}, resized; sessions and workspaces were kept`
          : `${v.name} resized; sessions and workspaces were kept`)
      await refreshIsolations()
      await refreshAgents()
    } catch (x) {
      if (x instanceof Unauthorized) { S.authed = false; render(); return }
      err.textContent = x.message
      save.disabled = false
      save.textContent = 'Save'
    }
  })
  return h('div', { class: 'subform' },
    h('div', { class: 'field' }, h('label', null, 'Name'), name,
      h('span', { class: 'hint' }, 'Only the name changes; agents keep running.')),
    h('div', { class: 'row spaced' },
      h('div', { class: 'field' }, h('label', null, 'CPUs'), cpus),
      h('div', { class: 'field' }, h('label', null, 'Memory'), memory, h('span', { class: 'hint' }, 'Like 8G. A new size replaces the container.')),
      h('div', { class: 'field' }, h('label', null, 'Swap'), swap, h('span', { class: 'hint' }, 'Like 8G. The first replaces the container; later ones go on in the running VM.'))),
    err,
    h('div', { class: 'actions' }, save, h('button', { class: 'btn', onclick: closeForm }, 'Cancel')))
}

// ---- Services: the credential proxies' environments

const DEFAULT = 'default'
const enc = encodeURIComponent
const envPath = (svc, name) => `/v1/envs/${enc(svc)}/${enc(name)}`

function rerender() {
  if (S.view === 'services') renderServices()
  else if (S.view === 'isolations') renderIsolations()
  else if (S.view === 'setup') renderSetup()
}
function openForm(key) { S.openForm = key; rerender() }
function closeForm() { S.openForm = null; S.login = null; S.checkOut = null; S.verifyOut = null; rerender() }

// fieldControls draws a service's fields, the same list the CLI builds its
// flags from, and reads them back as the CLI's edit: what was given is set,
// a list emptied is cleared, the rest is left alone.
// An isolations field (mail's reach) offers the other isolations by name;
// self is the isolation the form is for.
function fieldControls(fields, cur, adding, lookup, self) {
  cur = cur || {}
  const rows = []
  const readers = []
  for (const f of fields || []) {
    const v = cur[f.key]
    let ctl
    let read
    if (f.lookup && lookup && (f.kind === 'list' || (f.kind === 'string' && !f.choices?.length))) {
      const lc = lookup(f, v)
      ctl = lc.el
      read = lc.read
    } else if (f.kind === 'bool') {
      const opts = adding || typeof v !== 'boolean' ? [DEFAULT, 'on', 'off'] : ['on', 'off']
      const sel = typeof v === 'boolean' ? (v ? 'on' : 'off') : DEFAULT
      ctl = dropdown(opts, { value: sel, label: f.key })
      read = (p) => { if (ctl.value === 'on') p.set[f.key] = true; else if (ctl.value === 'off') p.set[f.key] = false }
    } else if (f.isolations && !S.isolations.some((i) => i.name !== self)) {
      ctl = h('span', { class: 'hint' }, 'there is no other isolation')
      read = () => {}
    } else if (f.kind === 'list' && (f.choices?.length || f.isolations)) {
      const have = new Set(Array.isArray(v) ? v : [])
      const choices = f.isolations ? S.isolations.map((i) => i.name).filter((n) => n !== self) : f.choices
      const boxes = choices.map((c) => ({ c, el: h('input', { type: 'checkbox', checked: have.has(c) }) }))
      ctl = h('div', { class: 'ticks' }, boxes.map(({ c, el }) => h('label', { class: 'check' }, el, c)))
      read = (p) => {
        const got = boxes.filter((b) => b.el.checked).map((b) => b.c)
        if (got.length) p.set[f.key] = got
        else if (have.size) p.clear.push(f.key)
      }
    } else if (f.kind === 'list') {
      const have = Array.isArray(v) ? v : []
      ctl = h('input', { value: have.join(', '), placeholder: (f.metavar || 'value') + ', …', autocomplete: 'off' })
      read = (p) => {
        const got = ctl.value.split(',').map((x) => x.trim()).filter(Boolean)
        if (got.length) p.set[f.key] = got
        else if (have.length) p.clear.push(f.key)
      }
    } else if (f.kind === 'string' && f.choices?.length) {
      ctl = dropdown([DEFAULT, ...f.choices], { value: v || DEFAULT, label: f.key })
      read = (p) => { if (ctl.value !== DEFAULT) p.set[f.key] = ctl.value }
    } else if (f.kind === 'int') {
      ctl = h('input', { type: 'number', value: typeof v === 'number' ? String(v) : '', placeholder: f.metavar || '' })
      read = (p) => {
        if (!ctl.value.trim()) return
        const n = Number(ctl.value)
        if (!Number.isInteger(n)) throw new Error(`${f.key} must be a whole number`)
        p.set[f.key] = n
      }
    } else {
      // A value stored in a shape the field does not spell (git's pinned
      // token command) is left as it is unless something is typed.
      const odd = v != null && typeof v !== 'string'
      ctl = h('input', { value: typeof v === 'string' ? v : '', placeholder: odd ? '(unchanged)' : f.metavar || '', autocomplete: 'off' })
      read = (p) => { const t = ctl.value.trim(); if (t) p.set[f.key] = t }
    }
    rows.push(h('div', { class: 'field' }, h('label', null, f.key), ctl, f.help ? h('span', { class: 'hint' }, f.help) : null))
    readers.push({ key: f.key, read })
  }
  // patch reads the form; skip leaves one field out (a lookup asking with
  // what the rest of the form says).
  return { rows, patch: (skip) => { const p = { set: {}, clear: [] }; readers.forEach((r) => { if (r.key !== skip) r.read(p) }); return p } }
}

// lookupControl is a List lookup field: the choices the service finds
// (the repositories a token opens, an account's instances), to tick. An
// entry of a field with caps narrows itself in place, spelled
// NAME=CAP,CAP. What the service cannot list is typed instead, and why is
// said. lookupRequest() is the request for what the form has right now.
function lookupControl(s, f, have, lookupRequest) {
  have = Array.isArray(have) ? have : []
  const box = h('div', { class: 'lookup' })
  const caps = (f.caps || []).map((c) => c.name)
  // name → the capabilities it keeps; null keeps them all.
  const chosen = new Map()
  const parse = (v) => {
    const i = caps.length ? v.indexOf('=') : -1
    if (i < 0) return [v, null]
    const keep = v.slice(i + 1).split(',').map((x) => x.trim()).filter(Boolean)
    // A capability the field does not have stays part of the name, for
    // the service to refuse, rather than read as all of them.
    if (!keep.length || keep.some((c) => !caps.includes(c))) return [v, null]
    return [v.slice(0, i), caps.filter((c) => keep.includes(c))]
  }
  const spell = ([name, keep]) => keep && keep.length < caps.length ? `${name}=${keep.join(',')}` : name
  const setFrom = (list) => { chosen.clear(); for (const v of list) { const [n, k] = parse(v); chosen.set(n, k) } }
  const split = (t) => t.split(',').map((x) => x.trim()).filter(Boolean)
  setFrom(have)
  let choices = null
  let asked = null
  const manual = h('input', { placeholder: `${f.metavar || 'value'}, … not listed above`, autocomplete: 'off' })
  const typed = (why) => {
    const input = h('input', { value: [...chosen].map(spell).join(', '), placeholder: `${f.metavar || 'value'}, …`, autocomplete: 'off' })
    input.addEventListener('input', () => setFrom(split(input.value)))
    box.replaceChildren(input, h('span', { class: 'hint warn' }, `${why} · typed instead, comma-separated`))
  }
  const row = (c) => {
    const cb = h('input', { type: 'checkbox', checked: chosen.has(c.value) })
    const boxes = (f.caps || []).map((cap) => {
      const el = h('input', { type: 'checkbox' })
      return { name: cap.name, el, label: h('label', { class: 'cap', title: cap.help || null }, el, cap.name) }
    })
    const capRow = boxes.length ? h('div', { class: 'caps' }, boxes.map((b) => b.label)) : null
    const sync = () => {
      if (!capRow) return
      const keep = chosen.get(c.value)
      for (const b of boxes) b.el.checked = !keep || keep.includes(b.name)
      capRow.hidden = !chosen.has(c.value)
    }
    cb.addEventListener('change', () => { if (cb.checked) chosen.set(c.value, null); else chosen.delete(c.value); sync() })
    for (const b of boxes) {
      b.el.addEventListener('change', () => {
        const on = boxes.filter((x) => x.el.checked).map((x) => x.name)
        // The last one stays: an entry with none is an entry unticked.
        if (!on.length) { b.el.checked = true; return }
        chosen.set(c.value, on.length === caps.length ? null : on)
      })
    }
    sync()
    return { c, el: h('div', { class: 'choice' },
      h('label', { class: 'check' }, cb, h('span', null, c.value), c.note ? h('span', { class: 'hint' }, c.note) : null), capRow) }
  }
  const draw = () => {
    const extra = [...chosen.keys()].filter((v) => !choices.some((c) => c.value === v)).map((v) => ({ value: v, note: 'not in the listing' }))
    const rows = [...extra, ...choices].map(row)
    const filter = h('input', { type: 'search', placeholder: `Filter ${choices.length} ${f.key}`, autocomplete: 'off' })
    filter.addEventListener('input', () => {
      const q = filter.value.trim().toLowerCase()
      for (const r of rows) r.el.hidden = !!q && !`${r.c.value} ${r.c.note || ''}`.toLowerCase().includes(q)
    })
    box.replaceChildren(...[
      choices.length > 8 ? filter : null,
      rows.length ? h('div', { class: 'choices' }, rows.map((r) => r.el)) : h('span', { class: 'hint' }, 'Nothing to list.'),
      manual].filter(Boolean))
  }
  const load = async () => {
    const req = lookupRequest()
    const key = JSON.stringify(req)
    if (key === asked) return
    asked = key
    box.replaceChildren(h('span', { class: 'hint' }, `Looking up ${f.key}…`))
    try {
      const got = await api('POST', `/v1/services/${enc(s.name)}/choices/${enc(f.key)}`, req)
      if (key !== asked) return
      choices = got
      draw()
    } catch (x) {
      if (x instanceof Unauthorized) { S.authed = false; render(); return }
      if (key !== asked) return
      choices = null
      typed(x.message)
    }
  }
  box.replaceChildren(h('span', { class: 'hint' }, `Looking up ${f.key}…`))
  return {
    el: box,
    load,
    read: (p) => {
      const more = choices ? split(manual.value) : []
      const got = [...new Set([...[...chosen].map(spell), ...more])].sort()
      if (got.length) p.set[f.key] = got
      else if (have.length) p.clear.push(f.key)
    },
  }
}

// lookupPick is a String lookup field: one of what the service finds (the
// Mac's AWS profiles, the roles one sees), or typed; what is typed narrows
// the list.
function lookupPick(s, f, v, lookupRequest) {
  const input = h('input', { value: typeof v === 'string' ? v : '', placeholder: f.metavar || '', autocomplete: 'off' })
  const list = h('div', { class: 'choices' })
  const hint = h('span', { class: 'hint' })
  const box = h('div', { class: 'lookup' }, input, list, hint)
  let choices = []
  let asked = null
  const draw = () => {
    const t = input.value.trim()
    const q = t.toLowerCase()
    const shown = !q || choices.some((c) => c.value === t) ? choices
      : choices.filter((c) => `${c.value} ${c.note || ''}`.toLowerCase().includes(q))
    list.hidden = !shown.length
    list.replaceChildren(...shown.map((c) => h('button', {
      type: 'button', class: 'pick' + (c.value === t ? ' on' : ''),
      // Picking is typing it: the rest of the form looks up again with it.
      onclick: () => { input.value = c.value; draw(); input.dispatchEvent(new Event('change', { bubbles: true })) },
    }, h('span', null, c.value), c.note ? h('span', { class: 'hint' }, c.note) : null)))
  }
  input.addEventListener('input', draw)
  const load = async () => {
    const req = lookupRequest()
    const key = JSON.stringify(req)
    if (key === asked) return
    asked = key
    hint.className = 'hint'
    hint.textContent = `Looking up ${f.key}…`
    try {
      const got = await api('POST', `/v1/services/${enc(s.name)}/choices/${enc(f.key)}`, req)
      if (key !== asked) return
      choices = got
      hint.textContent = choices.length ? `${choices.length} listed: pick one, or type it` : 'Nothing listed: type it.'
    } catch (x) {
      if (x instanceof Unauthorized) { S.authed = false; render(); return }
      if (key !== asked) return
      choices = []
      hint.className = 'hint warn'
      hint.textContent = `${x.message} · type it instead`
    }
    draw()
  }
  draw()
  return { el: box, load, read: (p) => { const t = input.value.trim(); if (t) p.set[f.key] = t } }
}

// shownService is the service the detail shows: the address's, or the
// first.
const shownService = () => S.services.find((s) => s.name === S.svcSel) || S.services[0] || null

function renderServices() {
  const page = document.getElementById('services')
  if (!page) return
  if (!S.services.length) {
    page.classList.remove('chosen')
    page.replaceChildren(h('div', { class: 'empty' }, 'Loading…'))
    return
  }
  const shown = shownService()
  const chosen = !!S.svcSel && shown?.name === S.svcSel
  split(page, chosen, 'Services', grouped(S.services).map((g) => [
    h('div', { class: 'master-group label' }, g.name),
    g.services.map((s) => masterItem(svcHash(s.name), s === shown,
      h('span', { class: 'item-line' }, h('span', { class: 'item-name' }, s.name), h('span', { class: 'item-side' }, serviceCount(s))),
      serviceAttention(s))),
  ]), serviceDetail(shown))
}

// serviceAttention is what of a service's envs waits on the operator
// (skills with an update to review), under its name in the list.
function serviceAttention(s) {
  const n = S.envs.filter((e) => e.service === s.name && e.status?.action === 'update').length
  return n ? h('span', { class: 'item-sub warn' }, n === 1 ? '1 update available' : `${n} updates available`) : null
}

// serviceCount is what a service has defined: how many environments, or
// that it has none to define.
function serviceCount(s) {
  if (!s.fields?.length) return 'built in'
  const n = S.envs.filter((e) => e.service === s.name).length
  return n ? String(n) : '—'
}

function serviceDetail(s) {
  const envs = S.envs.filter((e) => e.service === s.name)
  const key = 'new:' + s.name
  return [
    backLink('#/services', 'Services'),
    h('div', { class: 'detail-head' },
      h('div', { class: 'grow' }, h('div', { class: 'label' }, SERVICE_GROUPS[serviceGroup(s)].name), h('h2', null, s.name)),
      s.fields?.length && S.openForm !== key ? h('button', { class: 'btn primary', onclick: () => openForm(key) }, '+ New environment') : null),
    h('p', { class: 'lead' }, cap(s.summary) + '.'),
    S.openForm === key ? block('New environment', s.name === 'skill' ? skillForm(s, null) : envForm(s, null)) : null,
    s.fields?.length
      ? block(`Environments · ${envs.length}`, s.name === 'skill' ? skillChecked(s, envs) : null,
        envs.length ? envs.map((e) => envCard(s, e)) : h('p', { class: 'note' }, 'None yet: an isolation turns this service on with one.'))
      : h('p', { class: 'note' }, 'Nothing to define: turn it on for an isolation.'),
    block('In isolations', serviceUse(s)),
  ]
}

// serviceUse is each isolation with what the service serves there; one
// opens the isolation's services.
function serviceUse(s) {
  if (!S.isolations.length) return h('p', { class: 'note' }, 'No isolations yet.')
  return h('div', { class: 'list' }, S.isolations.map((i) => {
    const x = (S.exposures[i.name] || []).find((x) => x.service === s.name) || { service: s.name, enabled: false, envs: [] }
    const what = !x.enabled ? 'off' : exposureText(s, x) || (x.forwards || []).map((f) => f.url).join(', ') || 'on'
    return navLink(isoHash(i.name, 'services'), 'list-row link',
      h('span', { class: 'list-main' }, h('strong', null, i.name), h('span', { class: 'list-sub' }, what)),
      h('span', { class: 'chip' + (x.enabled ? ' ok' : '') }, x.enabled ? 'On' : 'Off'),
      icon('chevron-right'))
  }))
}

function secretChip(s, e) {
  if (!s.secrets?.length) return null
  if (e.secret_error) return h('span', { class: 'chip error' }, 'secret unreadable')
  if (e.credential_elsewhere) return h('span', { class: 'chip' }, 'token command')
  if (e.missing_secrets?.length) return h('span', { class: 'chip blocked' }, s.login ? 'not logged in' : 'no secret')
  return h('span', { class: 'chip' }, s.login ? 'logged in' : 'secret set')
}

function envCard(s, e) {
  const k = (what) => `${what}:${s.name}:${e.name}`
  let open = null
  if (S.openForm === k('edit')) open = s.name === 'skill' ? skillForm(s, e) : envForm(s, e)
  else if (S.openForm === k('review') && S.checkOut) open = skillReview(s, e)
  else if (S.openForm === k('secret')) open = secretForm(s, e)
  else if (S.openForm === k('login') && S.login) open = loginForm(s, e)
  else if (S.openForm === k('check') && S.checkOut) {
    open = h('div', { class: 'out' + (S.checkOut.error ? ' bad' : '') },
      h('pre', null, (S.checkOut.output || '') + (S.checkOut.error ? '✗ ' + S.checkOut.error : '')),
      h('button', { class: 'btn', onclick: closeForm }, 'Close'))
  }
  return h('div', { class: 'env-card' },
    h('div', { class: 'env-head' },
      h('strong', null, e.name), secretChip(s, e), statusChips(e),
      e.exposed_to?.length
        ? [h('span', { class: 'note' }, 'on for'), e.exposed_to.map((n) => navLink(isoHash(n, 'services'), 'chip accent', n))]
        : h('span', { class: 'note' }, 'not on for any isolation')),
    s.name === 'skill' ? skillConfig(e) : envConfig(s, e.config),
    open || h('div', { class: 'actions' },
      e.status?.action === 'update' ? h('button', { class: 'btn primary', onclick: () => checkEnv(s, e, k('review')) }, 'Review update') : null,
      h('button', { class: 'btn', onclick: () => openForm(k('edit')) }, 'Edit'),
      s.secrets?.length ? h('button', {
        class: 'btn', onclick: () => s.login ? startLogin(s, e, k('login')) : openForm(k('secret')),
      }, s.login ? 'Log in' : 'Secret') : null,
      h('button', { class: 'btn', onclick: () => checkEnv(s, e, k('check')) }, 'Check'),
      h('button', {
        class: 'btn ghost danger', onclick: async () => {
          const on = !e.exposed_to?.length ? ''
            : s.replaces ? ` ${e.exposed_to.join(', ')} restart without it; running agents end.`
              : ` ${e.exposed_to.join(', ')} stop serving it.`
          if (!await ask({ title: `Remove ${s.name} env ${e.name}?`, body: `${on} Its secret stays until cleared.`.trim(), ok: 'Remove', danger: true })) return
          await guarded(async () => {
            await api('DELETE', envPath(s.name, e.name) + (on ? '?force=1' : ''))
            toast(`Removed ${s.name}/${e.name}`)
            await refreshServices()
          })
        },
      }, 'Remove')))
}

// envConfig is an env's definition in words, field by field in the
// service's order: a list as its entries, a flag as yes or no.
function envConfig(s, cfg) {
  const fields = s.fields || []
  const known = new Set(fields.map((f) => f.key))
  const shown = (f, v) => {
    if (typeof v === 'boolean') return v ? 'yes' : 'no'
    if (Array.isArray(v)) return f.kind === 'list' ? h('span', { class: 'tags' }, v.map((x) => h('code', { class: 'tag' }, x))) : h('code', null, v.join(' '))
    if (v && typeof v === 'object') return h('code', null, JSON.stringify(v))
    return typeof v === 'string' && /^[~/]/.test(v) ? h('code', null, v) : String(v)
  }
  const pairs = [...fields.map((f) => [f, cfg?.[f.key]]), ...Object.keys(cfg || {}).filter((k) => !known.has(k)).map((k) => [{ key: k }, cfg[k]])]
    .filter(([, v]) => v != null && v !== '' && !(Array.isArray(v) && !v.length))
    .map(([f, v]) => [f.key.replaceAll('_', ' '), shown(f, v)])
  return pairs.length ? kv(pairs) : h('p', { class: 'note' }, 'Nothing set: the service’s defaults.')
}

function envForm(s, e) {
  // Editing, the name is there to change too: the env is renamed, and its
  // secret and the isolations exposing it follow.
  const name = h('input', { placeholder: 'letters, digits, - and _', autocomplete: 'off', value: e ? e.name : '' })
  // The name it has on the Mac, once a save has renamed it and then failed.
  let was = e?.name
  // A service whose choices are looked up with its credential (git's
  // repositories) takes the secret here, first: the lookup needs it.
  const lookups = []
  const secrets = s.fields.some((f) => f.lookup) && s.secrets?.length
    ? s.secrets.map((f) => ({
      f,
      el: h('input', {
        type: 'password', autocomplete: 'new-password',
        placeholder: e && !e.missing_secrets?.length && !e.credential_elsewhere ? 'stored; type a new one to replace it' : '',
      }),
    }))
    : []
  let ctl = null
  // A lookup asks with the fields it reads (lookup_with) as the form has
  // them, so it asks again when one of those changes, and only then.
  const lookupRequest = (f) => {
    const set = ctl ? ctl.patch(f.key).set : {}
    return {
      env: e ? was : undefined,
      set: Object.fromEntries((f.lookup_with || []).filter((k) => k in set).map((k) => [k, set[k]])),
      secret: Object.fromEntries(secrets.map(({ f, el }) => [f, el.value.trim()]).filter(([, v]) => v)),
    }
  }
  ctl = fieldControls(s.fields, e ? e.config : {}, !e, (f, v) => {
    const lc = (f.kind === 'list' ? lookupControl : lookupPick)(s, f, v, () => lookupRequest(f))
    lookups.push(lc)
    return lc
  })
  let pending = null
  const relook = () => { clearTimeout(pending); pending = setTimeout(() => lookups.forEach((l) => l.load()), 500) }
  for (const { el } of secrets) el.addEventListener('input', relook)
  const err = h('p', { class: 'form-error' })
  const save = h('button', { class: 'btn primary' }, 'Save')
  save.addEventListener('click', async () => {
    err.textContent = ''
    const n = name.value.trim()
    if (!n) { err.textContent = 'A name is required.'; return }
    const renamed = e && n !== was
    if (renamed && e.exposed_to?.length && !await ask({
      title: `Rename ${s.name}/${was} to ${n}?`,
      body: `${e.exposed_to.join(', ')} serve it under the new name from now on; agents there pick it up once their turn ends.`
        + (s.replaces ? ' Their containers are replaced: running agents end and start again on their next message.' : ''),
      ok: 'Rename',
    })) return
    if (!renamed && s.replaces && e?.exposed_to?.length && !await ask({
      title: `Restart ${e.exposed_to.join(', ')} with it?`,
      body: 'A changed folder is mounted only when the VM starts, so the container of each isolation that has it is replaced: running agents end and start again on their next message.',
      ok: 'Save and restart',
    })) return
    const secret = Object.fromEntries(secrets.map(({ f, el }) => [f, el.value.trim()]))
    try {
      // The definition first, under the name it has: one it refuses is
      // not renamed either.
      let v = await api(e ? 'PATCH' : 'POST', envPath(s.name, e ? was : n), ctl.patch())
      if (renamed) {
        const r = await api('POST', envPath(s.name, was) + '/rename', { name: n })
        v = { ...r, warnings: [...(v.warnings || []), ...(r.warnings || [])] }
        was = n
      }
      if (Object.values(secret).some(Boolean)) await api('PUT', envPath(s.name, n) + '/secret', secret)
      S.openForm = null
      toast([`${s.name}/${n} saved`, ...(v.warnings || [])].join(' — '))
      await refreshServices()
      renderServices()
    } catch (x) {
      if (x instanceof Unauthorized) { S.authed = false; render(); return }
      err.textContent = x.message
    }
  })
  const form = h('div', { class: 'subform' },
    h('div', { class: 'field' }, h('label', null, 'name'), name,
      h('span', { class: 'hint' }, e ? 'what isolations expose; changing it renames the env, and its secret and exposures follow' : 'what isolations expose')),
    secrets.map(({ f, el }) => h('div', { class: 'field' }, h('label', null, f), el,
      h('span', { class: 'hint' }, 'Stored encrypted in the Mac’s credentials file, never shown again. The proxy uses it; the isolation never sees it.'))),
    ctl.rows, err,
    h('div', { class: 'actions' }, save, h('button', { class: 'btn', onclick: closeForm }, 'Cancel')))
  // The rest of the form can change what a lookup reads (git's token
  // command, aws's profile); one whose request is the same does not ask.
  form.addEventListener('change', relook)
  if (lookups.length) queueMicrotask(() => lookups.forEach((l) => l.load()))
  return form
}

function secretForm(s, e) {
  const inputs = s.secrets.map((f) => ({ f, el: h('input', { type: 'password', autocomplete: 'new-password' }) }))
  const err = h('p', { class: 'form-error' })
  const save = h('button', { class: 'btn primary' }, 'Save')
  save.addEventListener('click', async () => {
    err.textContent = ''
    const body = Object.fromEntries(inputs.map(({ f, el }) => [f, el.value]))
    try {
      await api('PUT', envPath(s.name, e.name) + '/secret', body)
      S.openForm = null
      toast(`Stored [${s.name} ${e.name}] on the Mac; proxies read it on their next request`)
      await refreshServices()
      renderServices()
    } catch (x) {
      if (x instanceof Unauthorized) { S.authed = false; render(); return }
      err.textContent = x.message
    }
  })
  return h('div', { class: 'subform' },
    inputs.map(({ f, el }) => h('div', { class: 'field' }, h('label', null, f), el)),
    h('p', { class: 'note' }, 'Written encrypted to the Mac’s credentials file and never shown again.'), err,
    h('div', { class: 'actions' }, save,
      h('button', {
        class: 'btn', onclick: async () => {
          if (!await ask({ title: `Delete the stored secret for ${s.name}/${e.name}?`, body: 'It is removed from the Mac’s credentials file.', ok: 'Delete', danger: true })) return
          await guarded(async () => { await api('DELETE', envPath(s.name, e.name) + '/secret'); S.openForm = null; toast('Secret removed'); await refreshServices(); renderServices() })
        },
      }, 'Clear'),
      h('button', { class: 'btn', onclick: closeForm }, 'Cancel')))
}

async function startLogin(s, e, key) {
  await guarded(async () => {
    S.login = await api('POST', envPath(s.name, e.name) + '/login', {})
    openForm(key)
  })
}

// The login lands the browser on a loopback address on the Mac, which a
// phone (or any other device) cannot reach: the page fails to load, and its
// address, pasted here, finishes the login.
function loginForm(s, e) {
  const l = S.login
  const pasted = h('input', { placeholder: `${l.redirect}?code=…`, autocomplete: 'off' })
  const err = h('p', { class: 'form-error' })
  const finish = h('button', { class: 'btn primary' }, 'Finish login')
  finish.addEventListener('click', async () => {
    err.textContent = ''
    if (!pasted.value.trim()) { err.textContent = 'Paste the address the browser landed on.'; return }
    try {
      await api('POST', envPath(s.name, e.name) + '/login/' + enc(l.id), { callback: pasted.value.trim() })
      S.openForm = null
      S.login = null
      toast(`Logged in; [${s.name} ${e.name}] is stored on the Mac`)
      await refreshServices()
      renderServices()
    } catch (x) {
      if (x instanceof Unauthorized) { S.authed = false; render(); return }
      err.textContent = x.message + ' — start the login again if it keeps failing.'
    }
  })
  return h('div', { class: 'subform' },
    h('p', null, '1. ', h('a', { href: l.url, target: '_blank', rel: 'noopener noreferrer' }, `Open the ${s.name} login`), ' and approve the access.'),
    h('p', null, '2. The page it sends you to will not load. Copy that page’s address and paste it here:'),
    h('div', { class: 'field' }, pasted), err,
    h('div', { class: 'actions' }, finish, h('button', { class: 'btn', onclick: closeForm }, 'Cancel')))
}

// statusChips is what a service knows of an env beyond its definition (a
// skill from GitHub: up to date, an update to review, changed on the Mac).
const CHIP_KIND = { ok: ' ok', warn: ' blocked', error: ' error', plain: '' }
function statusChips(e) {
  return (e.status?.chips || []).map((c) => h('span', { class: 'chip' + (CHIP_KIND[c.kind] ?? '') }, c.text))
}

// ---- skill: its own form and card. A skill comes from a folder of the
// Mac's or a public GitHub repository; one from GitHub says whether the
// repository has moved on since, and is updated only when asked.

const skillOrigin = (e) => e?.config?.origin || null
const skillWhere = (o) => o.path ? `${o.repo}/${o.path}` : o.repo
const shortSha = (sha) => (sha || '').slice(0, 7)
const githubURL = (o) => `https://github.com/${o.repo}/tree/${encodeURIComponent(o.ref).replaceAll('%2F', '/')}${o.path ? '/' + o.path : ''}`

function ago(t) {
  const s = Math.max(0, (Date.now() - new Date(t).getTime()) / 1000)
  if (s < 90) return 'just now'
  if (s < 5400) return `${Math.round(s / 60)} minutes ago`
  if (s < 129600) return `${Math.round(s / 3600)} hours ago`
  return `${Math.round(s / 86400)} days ago`
}

// skillChecked heads the skills' list when some come from GitHub: when
// they were last looked at, and a button to look again. A look the daemon
// started for this listing shows a moment later.
function skillChecked(s, envs) {
  const gh = envs.filter(skillOrigin)
  if (!gh.length) return null
  const times = gh.map((e) => e.status?.checked_at).filter(Boolean).map((t) => new Date(t).getTime())
  if (times.length < gh.length && !S.skillRelook) {
    S.skillRelook = setTimeout(async () => { await guarded(refreshServices); S.skillRelook = null }, 4000)
  }
  const btn = h('button', { class: 'btn small' }, 'Check now')
  btn.addEventListener('click', async () => {
    btn.disabled = true
    btn.textContent = 'Checking…'
    await guarded(async () => {
      for (const e of gh) await api('POST', envPath(s.name, e.name) + '/check')
      await refreshServices()
      renderServices()
      toast(`Checked ${plural(gh.length, 'skill')} against GitHub`)
    })
    btn.disabled = false
    btn.textContent = 'Check now'
  })
  return h('div', { class: 'skill-checked' },
    h('span', { class: 'note' }, times.length ? `GitHub sources checked ${ago(Math.min(...times))}` : 'Checking GitHub sources…'), btn)
}

function skillConfig(e) {
  const c = e.config || {}
  const o = skillOrigin(e)
  const mono = (t) => h('code', null, t)
  return kv([
    c.description ? ['description', c.description] : null,
    o ? ['from', h('a', { href: githubURL(o), target: '_blank', rel: 'noopener' }, mono(skillWhere(o)))] : null,
    o ? ['ref', [mono(o.ref), h('span', { class: 'note' }, /^[0-9a-f]{7,40}$/i.test(o.ref) && o.commit.startsWith(o.ref.toLowerCase()) ? ' · a commit, so it stays put' : ' · followed')]] : null,
    o ? ['kept', [mono(shortSha(o.commit)), h('span', { class: 'note' }, ` · ${plural(c.files || 0, 'file')} · fetched ${new Date(o.fetched_at).toLocaleDateString(undefined, { month: 'short', day: 'numeric' })}`)]] : null,
    !o && c.source ? ['source', [mono(c.source), h('span', { class: 'note' }, ' · a folder on the Mac; Edit copies it in again')]] : null,
    !o ? ['files', String(c.files ?? '?')] : null,
    e.status?.detail ? [e.status.action === 'update' ? 'update' : 'source', e.status.detail] : null,
  ])
}

// skillReview is the check's account of an update (what changes, file by
// file), and taking it: with --force when the kept copy was changed on the
// Mac since, which the operator is asked about first.
function skillReview(s, e) {
  const out = S.checkOut
  const here = (e.status?.chips || []).some((c) => c.text === 'changed on the Mac')
  const err = h('p', { class: 'form-error' })
  const take = async (force) => {
    err.textContent = ''
    try {
      const v = await api('PATCH', envPath(s.name, e.name), { set: force ? { update: true, force: true } : { update: true }, clear: [] })
      S.openForm = null
      S.checkOut = null
      const o = skillOrigin(v)
      toast([`${s.name}/${e.name} updated to ${shortSha(o?.commit)}` + (v.exposed_to?.length ? ` — ${v.exposed_to.join(', ')} have it` : ''), ...(v.warnings || [])].join(' — '))
      await refreshServices()
      renderServices()
    } catch (x) {
      if (x instanceof Unauthorized) { S.authed = false; render(); return }
      err.textContent = x.message
    }
  }
  return h('div', { class: 'subform' },
    h('div', { class: 'out' + (out.error ? ' bad' : '') }, h('pre', null, (out.output || '') + (out.error ? '✗ ' + out.error : ''))),
    h('p', { class: 'note' }, 'A skill’s scripts run as the agent in every isolation that has it: read what changes before you take it.'),
    err,
    h('div', { class: 'actions' },
      here
        ? h('button', { class: 'btn danger', onclick: () => take(true) }, 'Replace my edit and update')
        : h('button', { class: 'btn primary', onclick: () => take(false) }, 'Update'),
      h('button', { class: 'btn', onclick: closeForm }, 'Not now')))
}

// skillForm adds or edits a skill: from a folder of the Mac's, or from a
// GitHub repository, whose skills are listed to pick one; the name is the
// one its SKILL.md gives, filled in by the pick.
function skillForm(s, e) {
  const o = skillOrigin(e)
  let from = o || !e ? 'github' : 'folder'
  const taken = new Set(S.envs.filter((x) => x.service === 'skill' && x.name !== e?.name).map((x) => x.name))
  const name = e ? null : h('input', { id: 'skill-name', placeholder: 'the name its SKILL.md gives', autocomplete: 'off' })
  let named = false
  name?.addEventListener('input', () => { named = !!name.value.trim() })
  const repo = h('input', { id: 'skill-repo', class: 'mono', value: o ? skillWhere(o) : '', placeholder: 'owner/repo, or a github.com address', autocomplete: 'off', spellcheck: 'false' })
  const ref = h('input', { id: 'skill-ref', class: 'mono', value: o ? o.ref : '', placeholder: 'the default branch', autocomplete: 'off', spellcheck: 'false' })
  const dir = h('input', { id: 'skill-path', class: 'mono', value: e?.config?.source || '', placeholder: '/Users/you/skills/pdf', autocomplete: 'off', spellcheck: 'false' })
  const list = h('div', { class: 'skill-found', role: 'radiogroup', 'aria-label': 'Skills in the repository' })
  const listHint = h('span', { class: 'hint' })
  let choices = []
  let picked = null
  const drawList = () => {
    list.hidden = !choices.length
    list.replaceChildren(...choices.map((c) => {
      const off = taken.has(c.name) || (e && c.name !== e.name)
      const on = picked === c
      return h('button', {
        type: 'button', role: 'radio', 'aria-checked': on ? 'true' : 'false', class: 'skill-pick' + (on ? ' on' : ''), disabled: off,
        onclick: () => {
          picked = c
          if (name && !named) name.value = c.name
          drawList()
        },
      }, h('span', { class: 'dot', 'aria-hidden': 'true' }),
      h('span', { class: 'skill-pick-text' },
        h('span', { class: 'skill-pick-line' }, h('span', { class: 'skill-pick-name' }, c.name), h('span', { class: 'skill-pick-path' }, c.value)),
        c.note ? h('span', { class: 'hint' }, c.note) : null),
      taken.has(c.name) ? h('span', { class: 'chip' }, 'already added') : null)
    }))
  }
  let asked = null
  let pending = null
  const lookup = () => {
    clearTimeout(pending)
    pending = setTimeout(async () => {
      const set = { github: repo.value.trim() }
      if (ref.value.trim()) set.ref = ref.value.trim()
      const key = JSON.stringify(set)
      if (!set.github || key === asked) return
      asked = key
      listHint.className = 'hint'
      listHint.textContent = 'Looking in the repository…'
      try {
        const got = await api('POST', '/v1/services/skill/choices/github', { set })
        if (key !== asked) return
        choices = got || []
        const want = e ? e.name : name.value.trim()
        picked = choices.find((c) => c.name === want && !taken.has(c.name)) || (choices.length === 1 && !taken.has(choices[0].name) ? choices[0] : null)
        if (picked && name && !named) name.value = picked.name
        listHint.textContent = choices.length ? `${plural(choices.length, 'skill')} in ${choices[0].where}` : 'No skill found there.'
      } catch (x) {
        if (x instanceof Unauthorized) { S.authed = false; render(); return }
        if (key !== asked) return
        choices = []
        picked = null
        listHint.className = 'hint warn'
        listHint.textContent = x.message
      }
      drawList()
    }, 500)
  }
  repo.addEventListener('input', lookup)
  ref.addEventListener('input', lookup)
  const seg = (id, label) => h('button', { type: 'button', role: 'radio', class: 'btn opt' + (from === id ? ' on' : ''), 'aria-checked': from === id ? 'true' : 'false', onclick: () => { from = id; draw() } }, label)
  const body = h('div', { class: 'skill-from' })
  const draw = () => {
    body.replaceChildren(
      h('div', { class: 'field' }, h('label', null, 'from'),
        h('div', { class: 'seg', role: 'radiogroup', 'aria-label': 'Where the skill comes from' }, seg('folder', 'Folder on the Mac'), seg('github', 'GitHub'))),
      from === 'github'
        ? [h('div', { class: 'field-row' },
          h('div', { class: 'field grow' }, h('label', { for: 'skill-repo' }, 'repository'), repo,
            h('span', { class: 'hint' }, 'A public repository: owner/repo, owner/repo/PATH, or an address ending in /tree/REF/PATH')),
          h('div', { class: 'field' }, h('label', { for: 'skill-ref' }, 'ref'), ref,
            h('span', { class: 'hint' }, 'a branch is followed; a tag or commit stays put'))),
        h('div', { class: 'field' }, h('label', null, 'skill'), list, listHint)]
        : h('div', { class: 'field' }, h('label', { for: 'skill-path' }, 'path'), dir,
          h('span', { class: 'hint' }, 'the skill’s folder, SKILL.md at its top, to copy in')))
  }
  draw()
  drawList()
  if (repo.value.trim()) lookup()
  const err = h('p', { class: 'form-error' })
  const save = h('button', { class: 'btn primary' }, 'Save')
  save.addEventListener('click', async () => {
    err.textContent = ''
    const n = e ? e.name : name.value.trim()
    if (!n) { err.textContent = 'A name is required: pick a skill, or type the name its SKILL.md gives.'; return }
    const set = {}
    if (from === 'folder') {
      if (dir.value.trim()) set.path = dir.value.trim()
      else if (!e || o) { err.textContent = 'Give the skill’s folder.'; return }
    } else {
      const r = repo.value.trim()
      if (!r) { err.textContent = 'Give a repository.'; return }
      const sameRepo = o && r === skillWhere(o)
      const sameRef = o && ref.value.trim() === o.ref
      if (!sameRepo) {
        set.github = picked ? picked.value : r
        if (ref.value.trim()) set.ref = ref.value.trim()
      } else if (!sameRef && ref.value.trim()) set.ref = ref.value.trim()
    }
    save.disabled = true
    save.textContent = set.github || set.ref ? 'Fetching…' : 'Saving…'
    try {
      const v = await api(e ? 'PATCH' : 'POST', envPath(s.name, n), { set, clear: [] })
      S.openForm = null
      const vo = skillOrigin(v)
      toast([`${s.name}/${n} saved` + (vo ? ` from ${skillWhere(vo)} @ ${shortSha(vo.commit)}` : ''), ...(v.warnings || [])].join(' — '))
      await refreshServices()
      renderServices()
    } catch (x) {
      if (x instanceof Unauthorized) { S.authed = false; render(); return }
      err.textContent = x.message
      save.disabled = false
      save.textContent = 'Save'
    }
  })
  return h('div', { class: 'subform' },
    body,
    name ? h('div', { class: 'field narrow' }, h('label', { for: 'skill-name' }, 'name'), name, h('span', { class: 'hint' }, 'what isolations expose; the name its SKILL.md gives')) : null,
    err,
    h('div', { class: 'actions' }, save, h('button', { class: 'btn', onclick: closeForm }, 'Cancel')))
}

async function checkEnv(s, e, key) {
  toast(`Checking ${s.name}/${e.name}…`)
  await guarded(async () => {
    S.checkOut = await api('POST', envPath(s.name, e.name) + '/check')
    S.toast = null
    renderToast()
    openForm(key)
  })
}

// ---- Verify: an isolation's guarantees, measured now (psps iso verify)

async function verifyIsolation(i) {
  S.verifying = i.name
  rerender()
  await guarded(async () => {
    S.verifyOut = await api('POST', `/v1/isolations/${enc(i.name)}/verify`)
    S.openForm = `verify:${i.name}`
  })
  S.verifying = null
  rerender()
}

// verifyView is a report: FAIL is a broken guarantee, WARN something set
// up wrong, each with its reason.
function verifyView(v) {
  const failed = v.checks.filter((c) => !c.ok)
  const broken = failed.filter((c) => c.severity === 'isolation')
  const kind = (c) => c.ok ? 'pass' : c.severity === 'isolation' ? 'fail' : 'warn'
  const summary = failed.length === 0 ? `${v.checks.length} checks passed`
    : `${failed.length} of ${v.checks.length} failed` + (broken.length ? ` — ${broken.length} broken guarantee${broken.length > 1 ? 's' : ''}` : ' — setup only')
  return h('div', { class: 'subform verify' },
    h('div', { class: 'verify-sum ' + (broken.length ? 'fail' : failed.length ? 'warn' : 'pass') }, summary),
    h('ul', { class: 'checks' }, v.checks.map((c) => h('li', { class: kind(c) },
      h('span', { class: 'mark' }, { pass: 'PASS', fail: 'FAIL', warn: 'WARN' }[kind(c)]),
      h('span', { class: 'what' }, c.check),
      h('span', { class: 'got' }, c.got),
      !c.ok && c.detail ? h('div', { class: 'why' }, c.detail) : null))),
    h('div', { class: 'actions' }, h('button', { class: 'btn', onclick: closeForm }, 'Close')))
}

// ---- An isolation's services

// forwardLinks is where a forwarded port is on the Mac, as a link where a
// browser opens it. noopener: the isolation's page gets no hold on this tab.
function forwardLinks(fs) {
  return fs.map((f, n) => [n ? ', ' : '',
    f.http ? h('a', { href: f.url + '/', target: '_blank', rel: 'noopener noreferrer' }, f.url.replace(/^http:\/\//, '')) : f.url,
    ` → ${f.port}` + (f.http ? '' : ' tcp')])
}

function exposeForm(i, s, x) {
  const names = S.envs.filter((e) => e.service === s.name).map((e) => e.name)
  const have = new Set(x.envs || [])
  // A service an isolation has with one env (control) offers a choice of one.
  const boxes = names.map((n) => ({ n, el: h('input', s.one_env ? { type: 'radio', name: `env-${i.name}-${s.name}`, checked: have.has(n) } : { type: 'checkbox', checked: have.has(n) }) }))
  const def = dropdown([DEFAULT, ...names], { value: x.default_env || DEFAULT, label: 'Default' })
  const ctl = fieldControls(s.settings, x.settings || {}, !x.enabled, null, i.name)
  const noEnvs = !s.fields?.length
  const err = h('p', { class: 'form-error' })
  const apply = h('button', { class: 'btn primary' }, x.enabled ? 'Apply' : 'Turn on')
  apply.addEventListener('click', async () => {
    err.textContent = ''
    const envs = boxes.filter((b) => b.el.checked).map((b) => b.n)
    // A share is mounted only when the VM starts: a change replaces it.
    const changed = !x.enabled || envs.length !== have.size || envs.some((n) => !have.has(n))
    if (s.replaces && changed && !await ask({
      title: `Restart ${i.name} with ${envs.length ? envs.map((n) => '/shared/' + n).join(', ') : 'no shared folder'}?`,
      body: 'A folder is mounted only when the VM starts, so its container is replaced: volumes, sessions and workspaces are kept; running agents end and start again on their next message.',
      ok: 'Restart',
    })) return
    apply.disabled = true
    if (s.replaces && changed) apply.textContent = 'Restarting…'
    try {
      const got = await api('PUT', `/v1/isolations/${enc(i.name)}/services/${enc(s.name)}`,
        { envs, default_env: def.value === DEFAULT ? undefined : def.value, settings: ctl.patch() })
      S.openForm = null
      if (got.warning) toast(got.warning, true)
      else toast(s.replaces ? `${i.name} has ${envs.map((n) => '/shared/' + n).join(', ') || 'no shared folder'}`
        : s.forwards ? `${i.name} on the Mac: ${(got.forwards || []).map((f) => f.url).join(', ') || 'no port'}`
          : `${s.name} is on for ${i.name}; agents pick it up at their next idle moment`)
      await refreshIsolations()
      renderIsolations()
    } catch (e) {
      if (e instanceof Unauthorized) { S.authed = false; render(); return }
      err.textContent = e.message
      apply.disabled = false
      apply.textContent = x.enabled ? 'Apply' : 'Turn on'
    }
  })
  // A share says which folder of the Mac's each env is, a port where it is
  // on the Mac.
  const config = (n) => S.envs.find((e) => e.service === s.name && e.name === n)?.config || {}
  // An http port's own name, as portproxy.Hostname makes it.
  const forwardText = (n) => {
    const c = config(n)
    const name = c.mode === 'tcp' ? '127.0.0.1' : (n.toLowerCase().replaceAll('_', '-').replace(/^-+|-+$/g, '') || 'port') + '.localhost'
    return `${name}:${c.host_port || c.port} → ${c.port}`
  }
  const envNote = (n) => {
    if (s.name === 'skill') return h('span', { class: 'hint' }, config(n).description || '')
    if (s.forwards) return h('span', { class: 'hint' }, `${forwardText(n)} · `, h('span', { class: 'mode' }, config(n).mode || 'http'))
    if (!s.replaces) return null
    const c = config(n)
    return h('span', { class: 'hint' }, `${c.path || '?'} · `, h('span', { class: 'mode' }, c.read_write ? 'read-write' : 'read-only'))
  }
  return h('div', { class: 'subform' },
    noEnvs ? null : h('div', { class: 'field' }, h('label', null, 'Environments'),
      names.length ? h('div', { class: 'ticks' }, boxes.map(({ n, el }) => h('label', { class: 'check' }, el, n, envNote(n))))
        : h('span', { class: 'hint' }, 'none defined yet — ', navLink(svcHash(s.name), null, 'define one')),
      h('span', { class: 'hint' }, s.replaces ? 'Each ticked folder is at /shared/NAME in the isolation.'
        : s.name === 'skill' ? 'Each ticked skill is in Claude Code’s and Codex’s skills there; an untick removes it.'
        : s.forwards ? "Each ticked port is on the Mac's 127.0.0.1, never the LAN. The container stays."
        : s.one_env ? 'One: what its agents may do.'
          : 'None ticked: the service with no credential (git: public repositories only).')),
    !noEnvs && !s.replaces && !s.forwards && !s.one_env && s.name !== 'skill' && names.length > 1 ? h('div', { class: 'field' }, h('label', null, 'Default'), def, h('span', { class: 'hint' }, 'what the plain variables point at')) : null,
    ctl.rows, err,
    h('div', { class: 'actions' }, apply,
      x.enabled ? h('button', { class: 'btn danger', onclick: () => turnOff(i, s) }, 'Turn off') : null,
      h('button', { class: 'btn', onclick: closeForm }, 'Cancel')))
}

function loginView() {
  const tok = h('input', { type: 'password', placeholder: 'token', autocomplete: 'off' })
  const err = h('p', { class: 'form-error' })
  const go = async () => {
    const res = await fetch('/v1/session', { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ token: tok.value }) })
    if (!res.ok) { err.textContent = 'That token is not valid.'; return }
    S.authed = true
    render()
    guarded(refreshAgents)
  }
  tok.addEventListener('keydown', (e) => { if (e.key === 'Enter') go() })
  return h('div', { class: 'login' },
    h('h2', null, 'Sign in to psps'),
    h('p', { class: 'note' }, 'Run ', h('code', null, 'psps web'), ' on the Mac: it opens a signed-in link, and prints one for your phone.'),
    h('div', { class: 'field' }, h('label', null, 'Or paste the token'), tok),
    err,
    h('button', { class: 'btn primary', onclick: go }, 'Sign in'))
}

// ---- Claude & Codex: each isolation's agent CLIs and where their
// credential comes from. An account is defined once, on the Mac, and any
// number of isolations use it (a Claude token, an API key). Otherwise the
// daemon runs a login inside the isolation: Claude's ends on a page showing
// a code to paste back here, Codex's asks for a code to be entered in the
// browser and notices by itself.

const cliName = (t) => (t === 'codex' ? 'Codex' : 'Claude Code')
const liveLogin = (l) => !!l && ['starting', 'open', 'verifying'].includes(l.state)
const cliPath = (iso, tool) => `/v1/isolations/${enc(iso)}/clis/${enc(tool)}`
const onScreen = (iso, tool) => !!S.cliLogin && S.cliLogin.iso === iso && S.cliLogin.tool === tool
const INSIDE = ''

async function refreshAccounts() {
  const [accounts, kinds] = await Promise.all([api('GET', '/v1/accounts'), S.accountKinds.length ? S.accountKinds : api('GET', '/v1/account-kinds')])
  S.accounts = accounts || []
  S.accountKinds = kinds || []
  if (!editing(S.view)) rerender()
}

async function refreshCLIs(names) {
  for (const n of names) S.clis[n] = null
  if (!editing(S.view)) rerender()
  await Promise.all(names.map(async (n) => {
    try {
      S.clis[n] = await api('GET', `/v1/isolations/${enc(n)}/clis`)
    } catch (e) {
      if (e instanceof Unauthorized) throw e
      S.clis[n] = { error: e.message }
    }
  }))
  // A login started elsewhere (the TUI, the CLI) is picked up here.
  if (!S.cliLogin) {
    for (const [iso, cs] of Object.entries(S.clis)) {
      const c = Array.isArray(cs) && cs.find((c) => liveLogin(c.login))
      if (c) { showCLILogin(iso, c.tool, c.login); break }
    }
  }
  if (!editing(S.view)) rerender()
}

function showCLILogin(iso, tool, login) {
  const first = !onScreen(iso, tool)
  S.cliLogin = { iso, tool, login }
  if (first) pollCLILogin(iso, tool)
}

async function pollCLILogin(iso, tool) {
  while (onScreen(iso, tool) && liveLogin(S.cliLogin.login)) {
    await new Promise((r) => setTimeout(r, 1500))
    if (!onScreen(iso, tool) || document.hidden) continue
    let l
    try {
      l = await api('GET', cliPath(iso, tool) + '/login')
    } catch (e) {
      if (e instanceof Unauthorized) { S.authed = false; render(); return }
      l = { state: 'failed', message: e.message }
    }
    if (!onScreen(iso, tool)) return
    const was = S.cliLogin.login.state
    S.cliLogin.login = l
    if (!liveLogin(l)) { cliLoginEnded(iso, tool, l); return }
    if (l.state !== was) rerender()
  }
}

function cliLoginEnded(iso, tool, l) {
  if (!onScreen(iso, tool)) return
  S.cliLogin = null
  if (l.state === 'done') toast(`${cliName(tool)} is logged in in ${iso}; its idle agents restart with it`)
  else toast(`${cliName(tool)} login: ${l.message || l.state}`, true)
  rerender()
  guarded(() => refreshCLIs([iso]))
}

async function startCLILogin(iso, tool) {
  if (S.cliLogin) await cancelCLILogin()
  S.cliBusy = `${iso}/${tool}`
  rerender()
  await guarded(async () => {
    const l = await api('POST', cliPath(iso, tool) + '/login')
    if (liveLogin(l)) showCLILogin(iso, tool, l)
    else toast(`${cliName(tool)} login: ${l.message || l.state}`, true)
  })
  S.cliBusy = null
  rerender()
}

async function cancelCLILogin() {
  const cur = S.cliLogin
  if (!cur) return
  S.cliLogin = null
  rerender()
  await guarded(() => api('DELETE', cliPath(cur.iso, cur.tool) + '/login'))
}

async function cliLogout(iso, tool) {
  if (!await ask({ title: `Log ${cliName(tool)} out in ${iso}?`, body: 'Its agents there stop working until it logs in again.', ok: 'Log out', danger: true })) return
  S.cliBusy = `${iso}/${tool}`
  rerender()
  await guarded(async () => {
    await api('POST', cliPath(iso, tool) + '/logout')
    toast(`${cliName(tool)} is logged out in ${iso}`)
  })
  S.cliBusy = null
  await guarded(() => refreshCLIs([iso]))
}

function copyButton(text, label) {
  return h('button', {
    class: 'btn', onclick: async () => {
      try { await navigator.clipboard.writeText(text); toast('Copied') } catch { toast('The browser refused the clipboard; copy it by hand', true) }
    },
  }, label)
}

const clock = (t) => new Date(t).toLocaleTimeString([], { hour: '2-digit', minute: '2-digit' })

// shownSetup is what the Accounts detail shows: the address's isolation,
// account or new account, else the first isolation's logins.
function shownSetup() {
  const sel = S.setupSel
  if (sel?.kind === 'new') return sel
  if (sel?.kind === 'account' && (S.accounts == null || S.accounts.some((a) => a.name === sel.name))) return sel
  if (sel?.kind === 'iso' && S.isolations.some((i) => i.name === sel.name)) return sel
  return S.isolations.length ? { kind: 'iso', name: S.isolations[0].name } : null
}

function renderSetup() {
  const page = document.getElementById('setup')
  if (!page) return
  const shown = shownSetup()
  const chosen = !!S.setupSel && shown === S.setupSel
  const is = (kind, name) => shown?.kind === kind && shown.name === name
  split(page, chosen, 'Accounts', [
    h('div', { class: 'master-head' }, h('span', { class: 'label' }, 'Logins by isolation')),
    S.isolations.length
      ? S.isolations.map((i) => masterItem(setupHash('iso', i.name), is('iso', i.name),
        h('span', { class: 'item-line' }, h('strong', null, i.name)), cliStates(i)))
      : h('p', { class: 'note master-note' }, 'No isolations yet.'),
    h('div', { class: 'master-head' }, h('span', { class: 'label' }, 'Shared accounts'), navLink(setupHash('new'), 'btn small', '+ New')),
    S.accounts == null ? h('p', { class: 'note master-note' }, 'Reading…')
      : S.accounts.length ? S.accounts.map((a) => masterItem(setupHash('account', a.name), is('account', a.name),
        h('span', { class: 'item-line' }, h('strong', null, a.name), h('span', { class: 'item-side' }, a.kind)),
        h('span', { class: 'item-sub' + (a.secret_error ? ' warn' : '') }, a.secret_error ? 'no secret' : a.used_by?.length ? 'used by ' + a.used_by.join(', ') : 'unused')))
        : h('p', { class: 'note master-note' }, 'None yet.'),
  ], setupDetail(shown))
}

// cliStates is how each agent CLI of an isolation is logged in, in a word.
function cliStates(i) {
  const cs = S.clis[i.name]
  if (cs == null) return h('span', { class: 'item-sub' }, 'reading…')
  if (!Array.isArray(cs)) return h('span', { class: 'item-sub warn' }, cs.error)
  return h('span', { class: 'item-sub states' }, cs.map((c) => {
    const ok = c.logged_in && !c.error
    return h('span', { class: 'state' + (ok ? '' : ' warn') }, h('span', { class: 'dot' + (ok ? ' on' : ' bad') }),
      cliName(c.tool) + (c.error ? ' unknown' : ok ? '' : ' not logged in'))
  }))
}

function setupDetail(sel) {
  if (!sel) {
    return [h('div', { class: 'detail-head' }, h('h2', null, 'Accounts')),
      h('p', { class: 'note' }, 'No isolations yet. ', navLink('#/isolations/' + NEW, null, 'Create one'), '.')]
  }
  if (sel.kind === 'new') {
    return [backLink('#/setup', 'Accounts'),
      h('div', { class: 'detail-head' }, h('h2', null, 'New account')),
      h('p', { class: 'lead' }, 'Defined once, on the Mac; any number of isolations can use one. A ChatGPT login cannot be shared this way (its refresh token is single-use), so Codex logs in inside each isolation.'),
      newAccountForm()]
  }
  if (sel.kind === 'account') return accountDetail(sel.name)
  const i = S.isolations.find((i) => i.name === sel.name)
  return [backLink('#/setup', 'Accounts'),
    h('div', { class: 'detail-head' }, h('h2', null, i.name),
      h('span', { class: 'chip ' + (isoUp(i) ? 'ok' : 'blocked') }, isoState(i))),
    h('p', { class: 'lead' }, 'What Claude Code and Codex log in with here: an account kept on the Mac, or a login made inside the isolation, which any browser can finish, a phone’s included.'),
    cliCards(i)]
}

function accountDetail(name) {
  if (S.accounts == null) return [backLink('#/setup', 'Accounts'), h('p', { class: 'note' }, 'Reading the accounts…')]
  const a = S.accounts.find((a) => a.name === name)
  const kind = S.accountKinds.find((k) => k.name === a.kind)
  return [backLink('#/setup', 'Accounts'),
    h('div', { class: 'detail-head' }, h('h2', null, a.name), h('span', { class: 'chip' }, a.kind),
      a.secret_error ? h('span', { class: 'chip error' }, 'no secret') : null),
    kind?.summary ? h('p', { class: 'lead' }, cap(kind.summary)) : null,
    kv([['Used by', a.used_by?.length
      ? h('span', { class: 'tags' }, a.used_by.map((n) => navLink(setupHash('iso', n), 'chip accent', n)))
      : 'No isolation: choose it for a CLI on an isolation’s logins']]),
    S.openForm === 'acct-secret:' + a.name ? accountSecretForm(a) : h('div', { class: 'actions' },
      h('button', { class: 'btn', onclick: () => openForm('acct-secret:' + a.name) }, 'Replace secret'),
      h('button', {
        class: 'btn ghost danger', onclick: async () => {
          const used = a.used_by?.length ? ` ${a.used_by.join(', ')} go back to a login inside.` : ''
          if (!await ask({ title: `Remove account ${a.name}?`, body: `Its secret is deleted.${used}`, ok: 'Remove', danger: true })) return
          let gone = false
          await guarded(async () => { await api('DELETE', `/v1/accounts/${enc(a.name)}?force=1`); toast(`Account ${a.name} removed`); gone = true })
          if (!gone) return
          for (const n of a.used_by || []) delete S.clis[n]
          await guarded(refreshAccounts)
          go('#/setup')
        },
      }, 'Remove'))]
}

function cliRow(i, c) {
  const busy = S.cliBusy === `${i.name}/${c.tool}`
  const chip = c.error ? h('span', { class: 'chip error' }, 'unknown')
    : c.logged_in ? h('span', { class: 'chip ok' }, 'logged in')
      : h('span', { class: 'chip blocked' }, 'not logged in')
  const how = c.error || [c.method, c.email].filter(Boolean).join(' · ')
  let below
  if (onScreen(i.name, c.tool)) below = cliLoginForm(i.name, c.tool, S.cliLogin.login)
  else if (c.account) below = h('p', { class: 'note' }, `Its agents use the account ${c.account}.`)
  else {
    below = h('div', { class: 'actions' },
      h('button', { class: 'btn' + (c.logged_in ? '' : ' primary'), disabled: busy || i.state !== 'running', onclick: () => startCLILogin(i.name, c.tool) },
        busy ? 'Working…' : c.logged_in ? 'Log in again' : 'Log in'),
      c.logged_in ? h('button', { class: 'btn', disabled: busy, onclick: () => cliLogout(i.name, c.tool) }, 'Log out') : null)
  }
  return h('div', { class: 'cli' },
    h('div', { class: 'head' }, h('span', { class: 'cli-name' }, cliName(c.tool)), c.version ? h('span', { class: 'ver' }, c.version) : null, chip),
    how ? h('div', { class: 'facts' }, how) : null,
    accountPicker(i, c, busy),
    below)
}

// accountPicker chooses what the CLI uses in the isolation: an account of
// its kind, or a login inside. Drawn only for a CLI some account kind is for.
function accountPicker(i, c, busy) {
  if (!S.accountKinds.some((k) => k.tool === c.tool)) return null
  const mine = (S.accounts || []).filter((a) => a.tool === c.tool)
  const sel = dropdown([
    { value: INSIDE, label: 'A login inside the isolation' },
    ...mine.map((a) => ({ value: a.name, label: `Account ${a.name} (${a.kind})` })),
  ], { value: c.account || INSIDE, disabled: busy, label: `What ${cliName(c.tool)} uses in ${i.name}` })
  sel.addEventListener('change', async () => {
    const account = sel.value
    S.cliBusy = `${i.name}/${c.tool}`
    rerender()
    await guarded(async () => {
      await api('PUT', cliPath(i.name, c.tool) + '/account', { account })
      toast(account ? `${cliName(c.tool)} in ${i.name} uses ${account}; its idle agents restart with it` : `${cliName(c.tool)} in ${i.name} goes back to a login inside`)
    })
    S.cliBusy = null
    await guarded(refreshAccounts)
    await guarded(() => refreshCLIs([i.name]))
  })
  return h('div', { class: 'field picker' }, h('label', null, 'Uses'), sel,
    mine.length ? null : h('span', { class: 'hint' }, 'No account of its kind yet; add one above.'))
}

// newAccountForm adds one. What is typed lives in S, as polling redraws
// the page, until it is added or the page left.
function newAccountForm() {
  const d = S.acctDraft
  const name = h('input', { autocomplete: 'off', placeholder: 'e.g. claude', value: d.name })
  const kind = dropdown(S.accountKinds.map((k) => k.name), { label: 'Kind', value: d.kind || undefined })
  const hint = h('span', { class: 'hint' })
  const showHint = () => { hint.textContent = S.accountKinds.find((k) => k.name === kind.value)?.summary || '' }
  kind.addEventListener('change', () => { d.kind = kind.value; showHint() })
  showHint()
  const secret = h('input', { type: 'password', autocomplete: 'new-password', placeholder: 'sk-ant-…', value: d.secret })
  name.addEventListener('input', () => { d.name = name.value })
  secret.addEventListener('input', () => { d.secret = secret.value })
  const err = h('p', { class: 'form-error' }, d.error || '')
  const save = h('button', { class: 'btn primary' }, 'Add account')
  save.addEventListener('click', async () => {
    d.error = ''
    err.textContent = ''
    try {
      const n = name.value.trim()
      await api('POST', `/v1/accounts/${enc(n)}`, { kind: kind.value, secret: secret.value.trim() })
      S.acctDraft = { name: '', kind: null, secret: '' }
      toast(`Account ${n} added; choose it for a CLI in an isolation's logins`)
      await refreshAccounts()
      go(setupHash('account', n))
    } catch (x) {
      if (x instanceof Unauthorized) { S.authed = false; render(); return }
      d.error = x.message
      err.textContent = x.message
    }
  })
  return h('div', { class: 'subform' },
    h('div', { class: 'row' }, h('div', { class: 'field' }, h('label', null, 'Name'), name), h('div', { class: 'field' }, h('label', null, 'Kind'), kind, hint)),
    h('div', { class: 'field' }, h('label', null, 'Secret'), secret,
      h('span', { class: 'hint' }, 'A token from ', h('code', null, 'claude setup-token'), ' (run it on any machine with a browser), or an API key. Stored encrypted in the Mac’s credentials file, never shown again.')),
    err,
    h('div', { class: 'actions' }, save, navLink('#/setup', 'btn', 'Cancel')))
}

// accountSecretForm replaces an account's secret.
function accountSecretForm(a) {
  const secret = h('input', { type: 'password', autocomplete: 'new-password', placeholder: 'the new token or key' })
  const err = h('p', { class: 'form-error' })
  const save = h('button', { class: 'btn primary' }, 'Save')
  save.addEventListener('click', async () => {
    err.textContent = ''
    try {
      await api('PATCH', `/v1/accounts/${enc(a.name)}`, { secret: secret.value.trim() })
      S.openForm = null
      toast(`Replaced the secret of ${a.name}` + (a.used_by?.length ? `; idle agents in ${a.used_by.join(', ')} restart with it` : ''))
      await refreshAccounts()
      rerender()
    } catch (x) {
      if (x instanceof Unauthorized) { S.authed = false; render(); return }
      err.textContent = x.message
    }
  })
  return h('div', { class: 'subform' }, h('div', { class: 'field' }, secret), err,
    h('div', { class: 'actions' }, save, h('button', { class: 'btn', onclick: closeForm }, 'Cancel')))
}

function cliLoginForm(iso, tool, l) {
  const cancel = h('button', { class: 'btn', onclick: cancelCLILogin }, 'Cancel')
  if (l.state === 'starting' || l.state === 'verifying') {
    return h('div', { class: 'subform' }, h('p', null, l.state === 'starting' ? 'Starting the login…' : 'Checking the code…'), h('div', { class: 'actions' }, cancel))
  }
  const link = (text) => h('a', { href: l.url, target: '_blank', rel: 'noopener noreferrer' }, text)
  if (!l.needs_code) {
    return h('div', { class: 'subform' },
      h('p', null, '1. Open ', link(l.url.replace(/^https:\/\//, '')), ' and sign in.'),
      h('p', null, '2. Enter this code:'),
      h('div', { class: 'device-code' }, l.code),
      h('p', { class: 'note' }, `Waiting for the browser… the code expires at ${clock(l.expires)}.`),
      h('div', { class: 'actions' }, copyButton(l.code, 'Copy code'), cancel))
  }
  const code = h('input', { type: 'password', autocomplete: 'off', placeholder: 'code', 'aria-label': 'The code the login page shows' })
  const err = h('p', { class: 'form-error' })
  const finish = h('button', { class: 'btn primary' }, 'Finish login')
  const submit = async () => {
    err.textContent = ''
    const v = code.value.trim()
    if (!v) { err.textContent = 'Paste the code the login page ends on.'; return }
    finish.disabled = true
    finish.textContent = 'Checking…'
    try {
      const r = await api('POST', cliPath(iso, tool) + '/login/code', { code: v })
      if (liveLogin(r)) { if (onScreen(iso, tool)) { S.cliLogin.login = r; rerender() } }
      else cliLoginEnded(iso, tool, r)
    } catch (x) {
      if (x instanceof Unauthorized) { S.authed = false; render(); return }
      finish.disabled = false
      finish.textContent = 'Finish login'
      err.textContent = x.message
    }
  }
  finish.addEventListener('click', submit)
  code.addEventListener('keydown', (e) => { if (e.key === 'Enter') submit() })
  return h('div', { class: 'subform' },
    h('p', null, '1. ', link('Open the Claude login'), ' and sign in.'),
    h('p', null, '2. The page ends on a code. Paste it here:'),
    h('div', { class: 'field' }, code), err,
    h('div', { class: 'actions' }, finish, copyButton(l.url, 'Copy address'), cancel))
}

function renderToast() {
  document.querySelector('.toast')?.remove()
  if (S.toast) document.body.append(h('div', { class: 'toast' + (S.toast.err ? ' err' : ''), role: 'status' }, S.toast.msg))
}

document.addEventListener('keydown', (e) => {
  if (e.key === 'Escape' && S.drawer) { S.drawer = false; render() }
})

// ---- Keyboard shortcuts, taken only when no field has the keys: ? lists
// them. Letters are read by key position as well, so they work with a
// Korean layout on.

const shortcuts = [
  ['?', 'Show these shortcuts'],
  ['/', 'Write in the message box'],
  ['Esc', 'Leave the message box; out of it, stop the working agent (Enter stops, Esc keeps it)'],
  ['j / k', 'Next / previous agent'],
  ['Ctrl+d / Ctrl+u', 'Scroll the conversation half a page down / up'],
  ['Ctrl+f / Ctrl+b', 'Scroll the conversation a page down / up'],
  ['G', 'Go to the end of the conversation'],
  ['Space', 'Park the idle agent, or make it idle again'],
  ['1–9', 'Pick that option of the question the agent asks'],
  ['0', 'Answer the question in your own words (Esc leaves the box)'],
  ['Enter', 'Send the answers to the question'],
  ['n', 'New agent'],
  ['m', 'Mail of the agent'],
  ['c', 'Cron of the agent'],
  ['t', 'TODO of the agent'],
  ['e', 'Name and ID of the agent'],
  ['x', 'Archive the agent (Enter archives, Esc keeps it)'],
  ['g then i / s / a', 'Go to Isolations / Services / Accounts'],
]

function showShortcuts() {
  if (document.querySelector('dialog[open]')) return
  ask({
    title: 'Keyboard shortcuts', cancel: 'Close',
    extra: h('table', { class: 'shortcuts' }, shortcuts.map(([k, what]) =>
      h('tr', null, h('td', null, k === '/' ? h('kbd', null, k) : k.split(' ').map((p) => ['then', '/'].includes(p) ? ` ${p} ` : h('kbd', null, p))), h('td', null, what)))),
  })
}

// askArchive asks before archiving the agent, as its Archive does and x.
async function askArchive(a) {
  if (!await ask({ title: `Archive "${agentTitle(a)}"?`, body: 'Its history is kept.', ok: 'Archive' })) return
  await guarded(() => api('POST', `/v1/agents/${a.id}/archive`))
  S.sel = null
  await guarded(refreshAgents)
  render()
}

// askStop asks before interrupting the agent's turn, as Esc does.
async function askStop(a) {
  if (!await ask({ title: `Stop ${agentTitle(a)}?`, body: 'Its turn is interrupted; a message continues it.', ok: 'Stop' })) return
  await guarded(() => api('POST', `/v1/agents/${a.id}/interrupt`))
}

// ---- Settings: this browser's own; the theme is the system's, or one picked.

const THEMES = [['system', 'System'], ['light', 'Light'], ['dark', 'Dark']]
function savedTheme() {
  try { const t = localStorage.getItem('psps.theme'); return THEMES.some(([v]) => v === t) ? t : 'system' } catch { return 'system' }
}
// applyTheme sets data-theme for a picked one, and the browser's bar color
// with it; the system's leaves both to the media queries.
const themeMetas = [...document.querySelectorAll('meta[name="theme-color"]')].map((m) => [m, m.getAttribute('media'), m.content])
function applyTheme(theme) {
  if (theme === 'system') document.documentElement.removeAttribute('data-theme')
  else document.documentElement.dataset.theme = theme
  for (const [m, media, content] of themeMetas) {
    if (theme === 'system') { m.setAttribute('media', media); m.content = content }
    else { m.removeAttribute('media'); m.content = theme === 'dark' ? '#121216' : '#f7f7f9' }
  }
}
applyTheme(savedTheme())

function openSettings() {
  if (document.querySelector('dialog[open]')) return
  const seg = h('div', { class: 'segmented', role: 'radiogroup', 'aria-label': 'Theme' })
  const draw = () => {
    const now = savedTheme()
    seg.replaceChildren(...THEMES.map(([v, label]) => h('button', {
      type: 'button', role: 'radio', 'aria-checked': String(v === now), class: v === now ? 'on' : '',
      onclick: (e) => {
        e.preventDefault()
        try { localStorage.setItem('psps.theme', v) } catch {}
        applyTheme(v)
        draw()
        seg.querySelector('.on')?.focus()
      },
    }, label)))
  }
  draw()
  ask({
    title: 'Settings', cancel: 'Close',
    extra: h('div', { class: 'settings-list' },
      h('div', { class: 'setting' }, h('span', { class: 'setting-name' }, 'Theme'), seg),
      h('p', { class: 'note' }, 'Kept in this browser.'),
      h('hr', { class: 'settings-rule' }),
      autocompactSetting(),
      h('hr', { class: 'settings-rule' }),
      adminSetting()),
  })
}

// autocompactSetting is autocompact's part of Settings: its switch and,
// while it is on, after how many minutes, read from the daemon as Settings
// opens. Both are a draft until Apply saves them; Revert, or closing
// Settings, drops it.
function autocompactSetting() {
  const sw = h('button', { type: 'button', class: 'switch', role: 'switch', 'aria-checked': 'false', 'aria-label': 'Autocompact', disabled: true })
  const min = h('input', {
    id: 'autocompact-minutes', class: 'dialog-input', type: 'number', min: '1', step: '1', inputmode: 'numeric',
    'aria-describedby': 'autocompact-note',
  })
  const note = h('p', { id: 'autocompact-note', class: 'note' })
  const detail = h('div', { class: 'autocompact-detail', hidden: true },
    h('label', { class: 'autocompact-when', for: 'autocompact-minutes' }, h('span', null, 'After'), min, h('span', null, 'minutes since its last message')),
    note)
  const said = h('span', { class: 'autocompact-said', role: 'status' })
  const err = h('p', { class: 'form-error', role: 'alert', hidden: true })
  const revert = h('button', { type: 'button', class: 'btn small', hidden: true }, 'Revert')
  const apply = h('button', { type: 'button', class: 'btn small primary', disabled: true }, 'Apply')
  const foot = h('div', { class: 'autocompact-foot', hidden: true }, said, h('div', { class: 'autocompact-actions' }, revert, apply))
  const box = h('div', { class: 'admin-setting' },
    h('div', { class: 'setting admin-head' },
      h('div', { class: 'admin-text' },
        h('div', { class: 'setting-name' }, 'Autocompact'),
        h('div', { class: 'note' }, 'Send /compact to an agent that has been quiet for a while.')),
      sw),
    detail, foot, err)
  // cur is what the daemon has; draft what Settings shows, minutes as typed.
  let cur = null, draft = null, saving = false
  const typed = () => Number(draft.minutes)
  const valid = () => Number.isInteger(typed()) && typed() >= 1
  const dirty = () => draft.on !== cur.on || (draft.on && typed() !== cur.minutes)
  const words = (v) => v.on ? `On, after ${v.minutes} minute${v.minutes === 1 ? '' : 's'}` : 'Off'
  const draw = () => {
    sw.disabled = saving
    sw.classList.toggle('on', draft.on)
    sw.setAttribute('aria-checked', String(draft.on))
    detail.hidden = !draft.on
    if (document.activeElement !== min) min.value = draft.minutes
    note.replaceChildren('psps looks at every agent once a minute. One that is idle and whose last message is ', valid() ? String(typed()) : 'that many',
      ' minutes old gets ', h('code', null, '/compact'), ', unless its last message was /compact already. Agents a team lead made, and those in an open watch, are left alone.')
    const d = dirty()
    foot.hidden = false
    said.classList.toggle('pending', d)
    said.textContent = d ? `Not applied yet. Now: ${words(cur)}.` : `Applied: ${words(cur)}.`
    revert.hidden = !d
    apply.disabled = !d || saving || (draft.on && !valid())
  }
  const take = async () => {
    if (!cur || !dirty() || saving) return
    if (draft.on && !valid()) { err.textContent = 'A whole number of minutes, 1 or more.'; err.hidden = false; return }
    err.hidden = true
    saving = true
    draw()
    try {
      cur = await api('PUT', '/v1/autocompact', { on: draft.on, minutes: draft.on ? typed() : cur.minutes })
      draft = { ...cur }
      toast(`Autocompact: ${words(cur)}`)
    } catch (e) {
      err.textContent = e.message
      err.hidden = false
    }
    saving = false
    draw()
  }
  sw.addEventListener('click', (e) => {
    e.preventDefault()
    if (!draft) return
    draft.on = !draft.on
    err.hidden = true
    draw()
  })
  min.addEventListener('input', () => { if (draft) { draft.minutes = min.value; err.hidden = true; draw() } })
  // Enter in the dialog's form would close it: it applies instead.
  min.addEventListener('keydown', (e) => { if (e.key === 'Enter' && !e.isComposing) { e.preventDefault(); take() } })
  apply.addEventListener('click', (e) => { e.preventDefault(); take() })
  revert.addEventListener('click', (e) => {
    e.preventDefault()
    draft = { ...cur }
    min.value = cur.minutes
    err.hidden = true
    draw()
  })
  api('GET', '/v1/autocompact').then((v) => { cur = v; draft = { ...v }; draw() }).catch(() => {
    detail.hidden = false
    detail.replaceChildren(h('p', { class: 'form-error' }, "Can't read autocompact from the daemon."))
  })
  return box
}

// ---- Admin mode: an agent on the Mac itself, outside every isolation.

// tildeHome writes a path on the Mac under its home as ~.
function tildeHome(p) {
  const home = S.adminHome
  return home && p && (p + '/').startsWith(home + '/') ? '~' + p.slice(home.length) : p
}

// adminSetting is admin mode's part of Settings: its switch and, while it
// is on, what its agent is, read from the daemon as Settings opens.
function adminSetting() {
  const sw = h('button', { type: 'button', class: 'switch', role: 'switch', 'aria-checked': 'false', 'aria-label': 'Admin mode', disabled: true })
  const detail = h('div', { class: 'admin-detail', hidden: true })
  const box = h('div', { class: 'admin-setting' },
    h('div', { class: 'setting admin-head' },
      h('div', { class: 'admin-text' },
        h('div', { class: 'setting-name' }, 'Admin mode'),
        h('div', { class: 'note' }, 'Run Claude or Codex on this Mac, outside every isolation, as you.')),
      sw),
    detail)
  let cur = null
  const closeSettings = () => box.closest('dialog')?.close('cancel')
  const draw = () => {
    sw.disabled = false
    sw.classList.toggle('on', !!cur.on)
    sw.setAttribute('aria-checked', String(!!cur.on))
    detail.hidden = !cur.on
    if (!cur.on) return detail.replaceChildren()
    const a = agentById(cur.agent)
    const st = a ? (a.state?.status || 'idle') : 'starting'
    detail.replaceChildren(
      h('dl', { class: 'admin-card' },
        h('dt', null, 'Provider'), h('dd', null, cur.kind === 'codex' ? 'Codex' : 'Claude'),
        h('dt', null, 'Directory'), h('dd', { class: 'mono' }, tildeHome(cur.dir)),
        h('dt', null, 'Account'), h('dd', null, cur.account ? h('span', { class: 'mono' }, cur.account) : "This Mac's login"),
        h('dt', null, 'State'), h('dd', null, h('span', { class: 'chip ' + st }, st))),
      h('div', { class: 'admin-actions' },
        h('button', { type: 'button', class: 'btn small', onclick: (e) => { e.preventDefault(); closeSettings(); openAdminSetup(cur) } }, 'Change…'),
        h('button', { type: 'button', class: 'btn small', onclick: (e) => { e.preventDefault(); closeSettings(); select(cur.agent) } }, 'Open Admin')),
      h('p', { class: 'note' }, 'Turning it off stops Admin and archives it. Changing the directory or provider starts a new session; another account keeps it.'))
  }
  sw.addEventListener('click', async (e) => {
    e.preventDefault()
    if (!cur) return
    if (!cur.on) { closeSettings(); openAdminSetup(cur); return }
    closeSettings()
    if (!await ask({
      title: 'Turn admin mode off?',
      body: 'Admin stops, whatever it is doing, and is archived with its conversation. Turn it on again for a new one.',
      ok: 'Turn off', danger: true,
    })) return
    await guarded(async () => {
      await api('DELETE', '/v1/admin')
      if (S.sel === cur.agent) S.sel = null
      toast('Admin mode is off')
      await refreshAgents()
      render()
    })
  })
  api('GET', '/v1/admin').then((v) => { cur = v; S.adminHome = v.home; draw() }).catch(() => {
    detail.hidden = false
    detail.replaceChildren(h('p', { class: 'form-error' }, "Can't read admin mode from the daemon."))
  })
  return box
}

// openAdminSetup asks where admin's agent works and which CLI it is, and
// starts it: admin mode is on once it has.
function openAdminSetup(cur) {
  S.adminHome = cur.home
  const recents = (cur.recent || []).map(tildeHome)
  let kind = cur.on ? cur.kind : 'claude'
  const dir = h('input', {
    id: 'admin-dir', class: 'dialog-input mono', placeholder: '~/path/to/project', autocomplete: 'off', autocapitalize: 'off', spellcheck: 'false',
    value: cur.on ? tildeHome(cur.dir) : (recents[0] || ''),
  })
  const cmd = h('div', { class: 'dialog-cmd admin-cmd', 'aria-label': 'What runs' })
  const drawCmd = () => { cmd.textContent = `cd ${dir.value.trim() || '~'} && ${kind}` }
  const field = dirCompleter(dir, () => '', {
    list: (typed) => api('GET', `/v1/admin/dirs?path=${encodeURIComponent(typed)}`),
    recents: () => recents,
    recentTitle: 'An admin agent worked here before',
    missing: 'No such directory on this Mac.',
    onAccept: drawCmd,
  })
  const provs = h('div', { class: 'admin-provs', role: 'radiogroup', 'aria-label': 'Provider' })
  let drawProvs = () => provs.replaceChildren(...[['claude', 'Claude'], ['codex', 'Codex']].map(([k, label]) => h('button', {
    type: 'button', role: 'radio', 'aria-checked': String(k === kind), class: 'admin-prov' + (k === kind ? ' on' : ''),
    onclick: (e) => { e.preventDefault(); kind = k; drawProvs(); drawCmd() },
  }, h('span', { class: 'radio' }), h('span', { class: 'prov-text' }, h('span', { class: 'prov-name' }, label), h('span', { class: 'prov-cmd' }, k)))))
  // The account its CLI logs in with: a shared one from Accounts, or the
  // CLI's own login on the Mac (the Keychain, or what a login shell sets).
  let accounts = null
  let account = cur.on ? (cur.account || '') : ''
  const accts = h('div', { class: 'admin-provs', role: 'radiogroup', 'aria-label': 'Account' })
  const acctHint = h('p', { class: 'field-hint' })
  const drawAccts = () => {
    const mine = (accounts || []).filter((a) => a.tool === kind)
    if (!mine.some((a) => a.name === account)) account = ''
    const opt = (name, label, sub, bad) => h('button', {
      type: 'button', role: 'radio', 'aria-checked': String(name === account), class: 'admin-prov' + (name === account ? ' on' : ''),
      disabled: !!bad, title: bad || '',
      onclick: (e) => { e.preventDefault(); account = name; drawAccts() },
    }, h('span', { class: 'radio' }), h('span', { class: 'prov-text' }, h('span', { class: 'prov-name' + (name ? ' mono' : '') }, label), h('span', { class: 'prov-cmd' }, sub)))
    accts.replaceChildren(
      opt('', "This Mac's login", `${kind}'s own`),
      ...mine.map((a) => opt(a.name, a.name, a.kind, a.secret_error)))
    acctHint.textContent = accounts === null ? 'Reading accounts…'
      : mine.length ? 'A shared account from Settings › Accounts, or the login the CLI has on this Mac.'
      : kind === 'codex' ? 'Codex has no shared accounts (a ChatGPT login cannot be shared): it uses its login on this Mac.'
      : 'No Claude account in Settings › Accounts yet: it uses its login on this Mac.'
  }
  const drawKind = drawProvs
  drawProvs = () => { drawKind(); drawAccts() }
  api('GET', '/v1/accounts').then((v) => { accounts = v || []; drawAccts() }).catch(() => { accounts = []; drawAccts() })
  drawProvs()
  drawCmd()
  ask({
    title: cur.on ? 'Change admin agent' : 'Admin agent',
    body: 'An agent on this Mac itself, not in an isolation. It sits at the top of the sidebar while admin mode is on.',
    wide: true,
    extra: h('div', { class: 'admin-form' },
      h('label', { class: 'dialog-field', for: 'admin-dir' }, 'Working directory'),
      field,
      h('p', { class: 'field-hint' }, 'Tab completes · ↑↓ to move · directories on this Mac'),
      h('span', { class: 'dialog-field' }, 'Provider'),
      provs,
      h('span', { class: 'dialog-field' }, 'Account'),
      accts,
      acctHint,
      h('div', { class: 'admin-warn' }, icon('triangle-alert'),
        h('span', null, 'No isolation around it: it can read and change any file you can, and reach every isolation through psps. It asks before acting as your own settings for the CLI say.')),
      cmd),
    ok: cur.on ? 'Apply' : 'Start admin agent',
    onOk: async () => {
      if (!dir.value.trim()) throw new Error('Pick a working directory.')
      const v = await api('PUT', '/v1/admin', { kind, dir: dir.value.trim(), account })
      toast(!cur.on ? 'Admin mode is on' : v.agent !== cur.agent ? 'Admin started over'
        : (v.account || '') !== (cur.account || '') ? 'Admin restarts with the account when idle' : 'Admin is as it was')
      await refreshAgents()
      select(v.agent)
    },
  })
  queueMicrotask(() => { dir.focus(); dir.setSelectionRange(dir.value.length, dir.value.length) })
}

// adminRow is the admin agent in the sidebar, above every other: a row of
// its own, no team's, in no order but first.
function adminRow(a) {
  const st = a.state?.status || 'idle'
  const meta = a.problem ? h('span', { class: 'problem' }, icon('triangle-alert'), ' ' + a.problem)
    : `${st} · ${a.kind} · ${tildeHome(a.workdir)}`
  return h('button', {
    class: 'agent admin-row' + (a.id === S.sel && S.view === 'agent' ? ' selected' : ''),
    'data-line': a.line || a.id, onclick: () => select(a.id), title: 'Admin: runs on this Mac, outside every isolation',
  }, h('span', { class: 'admin-icon' }, icon('shield')),
    nameWithAge(a, null),
    h('span', { class: 'host-tag' }, 'HOST'),
    h('span', { class: 'meta' }, meta))
}

// stepAgent opens the agent by from the open one in the sidebar's live list,
// a team's agents with it where its fold shows them.
function stepAgent(by) {
  const live = document.querySelector('#sidebar .sortable:first-of-type')
  const rows = live ? [...live.querySelectorAll(':scope > .agent, :scope > .team .agent')] : []
  if (!rows.length) return
  const ids = rows.map((r) => S.agents.find((a) => !a.archived_at && (a.line || a.id) === r.dataset.line)?.id)
  const at = ids.indexOf(S.view === 'agent' ? S.sel : null)
  const next = ids[at < 0 ? (by > 0 ? 0 : ids.length - 1) : Math.min(Math.max(at + by, 0), ids.length - 1)]
  if (!next) return
  const line = rows[ids.indexOf(next)].dataset.line
  select(next)
  // select draws the sidebar anew: the row to bring into view is the new
  // one, the old having left the page.
  document.querySelector(`#sidebar .sortable:first-of-type .agent[data-line="${CSS.escape(line)}"]`)?.scrollIntoView({ block: 'nearest' })
}

// The tasks list closes on a click anywhere else.
document.addEventListener('click', (e) => { if (S.tasksOpen && !e.target.closest?.('.tasks-pop, .sl-tasks')) closeTasks() })
// So does the icon picker, which follows its row as the sidebar scrolls.
document.addEventListener('click', (e) => { if (S.iconFor && !e.target.closest?.('#icon-pop, [data-pick]')) closeIconPick(false) })
document.addEventListener('scroll', () => { if (S.iconFor) placeIconPick() }, true)
addEventListener('resize', () => { if (S.iconFor) placeIconPick() })

let goPending = 0
document.addEventListener('keydown', (e) => {
  const t = e.target
  if (e.key === 'Escape' && !e.isComposing && closeIconPick(true)) { e.preventDefault(); return }
  if (e.key === 'Escape' && !e.isComposing && closeTasks()) { e.preventDefault(); return }
  if (e.key === 'Escape' && !e.isComposing && S.pick && !document.querySelector('dialog[open]')) { e.preventDefault(); stopPicking(); return }
  // Esc leaves the message box, or a field of the new agent's form, once
  // nothing in it (a completion) took it: the shortcuts work again.
  if (e.key === 'Escape' && !e.defaultPrevented && !e.isComposing &&
    (t.matches?.('.composer textarea') || (S.view === 'new' && t.matches?.('.page input, .page textarea')))) { t.blur(); return }
  if (e.metaKey || e.altKey || e.isComposing || e.defaultPrevented) return
  if (t.closest?.('input, textarea, select, [contenteditable]') || document.querySelector('dialog[open]')) return
  const key = e.code?.startsWith('Key') ? e.code.slice(3).toLowerCase() : e.key
  // Ctrl with d / u / f / b scrolls the conversation as vim does: half a
  // page down / up, a page down / up.
  if (e.ctrlKey) {
    const box = S.view === 'agent' && !e.shiftKey && document.getElementById('convo')
    const by = box && { d: 0.5, u: -0.5, f: 0.9, b: -0.9 }[key]
    if (by) { e.preventDefault(); box.scrollBy({ top: by * box.clientHeight }) }
    return
  }
  const a = S.view === 'agent' ? agentById(S.sel) : null
  // A question the open agent waits on answers to the numbers and Enter.
  const asking = a && [...document.querySelectorAll('#convo .approval.asking')].find((c) => c.keys)
  if (asking && !e.shiftKey) {
    const n = /^Digit[0-9]$/.test(e.code) ? +e.code.slice(5) : /^[0-9]$/.test(e.key) ? +e.key : -1
    const answering = n === 0 ? () => asking.keys.write()
      : n > 0 ? () => asking.keys.pick(n)
      : e.key === 'Enter' && (t === document.body || asking.contains(t)) ? () => asking.keys.answer()
      : null
    if (answering) { e.preventDefault(); answering(); return }
  }
  const going = Date.now() - goPending < 1500
  goPending = 0
  const act = going ? { i: () => show('isolations'), s: () => show('services'), a: () => show('setup') }[key]
    : e.key === '?' ? showShortcuts
    : e.shiftKey && key === 'g' ? S.view === 'agent' && (() => {
      const box = document.getElementById('convo')
      if (box) box.scrollTop = box.scrollHeight
    })
    : e.shiftKey ? null
    : {
      '/': () => document.querySelector('.composer textarea:not(:disabled)')?.focus(),
      ' ': a && (a.archived_at || (a.state?.status || 'idle') !== 'idle' ? null : () => togglePark(a)),
      j: () => stepAgent(1),
      k: () => stepAgent(-1),
      n: () => show('new'),
      m: a && (() => openMail(a)),
      c: a && !a.archived_at && (() => openCrons(a)),
      t: a && !a.archived_at && (() => openTodos(a)),
      e: a && (() => editAgent(a)),
      x: a && !a.archived_at && (() => askArchive(a)),
      g: () => { goPending = Date.now() },
      // Out of the message box, Esc asks to stop the agent at work: the
      // dialog's Stop has the focus, so Enter stops and Esc closes it.
      Escape: a && !S.drawer && a.state?.status === 'working' && !a.archived_at && (() => askStop(a)),
    }[key]
  if (!act) return
  e.preventDefault()
  act()
})

route()
startPolling()
