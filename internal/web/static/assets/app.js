// gero web UI. Plain DOM, no framework, no build step.
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
    headers: { 'Content-Type': 'application/json', 'X-Gero-Request': '1' },
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
  // A sidebar row is held or being moved: the sidebar is not redrawn under
  // it, but once it is let go.
  sorting: false,
  sidebarStale: false,
  drawer: false,
  convos: new Map(),
  drafts: new Map(),
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
  openForm: null,
  login: null,
  checkOut: null,
  verifyOut: null, // the open verify report
  verifying: null, // the isolation being measured
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
  isoCreating: null,
  isoCreateError: null,
  // edit is the user message being rewritten in place: { agent, id, text,
  // sending }. openRewound holds the earlier versions shown unfolded,
  // openTools the runs of one tool likewise.
  edit: null,
  openRewound: new Set(),
  openTools: new Set(),
}

const coarse = matchMedia('(pointer: coarse)').matches
const agentById = (id) => S.agents.find((a) => a.id === id)
// agentTitle is what an agent is called on the page: one made without a
// prompt has no name until its first message.
const agentTitle = (a) => a.name || 'New agent'
// shownID is the ID an agent goes by: its own, the one it went by until a
// new session took it, or the key gero gave it.
const shownID = (a) => a.handle || a.was || a.id
// letter reads a mail from another agent as the agent got it (the daemon's
// mailproxy.Letter): who wrote it, from where, and what it says.
const LETTER = /^\[gero mail #(\d+) from agent (\S+) in isolation (\S+)\. [^\n]*\]\n\n/
function letter(text) {
  const m = LETTER.exec(text)
  return m && { id: m[1], from: m[2], iso: m[3], body: text.slice(m[0].length) }
}
// A cron's message as its agent got it (internal/daemon/crons.go).
const CRON = /^\[gero cron #(\d+), on schedule "([^"\n]*)"\. [^\n]*\]\n\n/
function cronMsg(text) {
  const m = CRON.exec(text)
  return m && { id: m[1], schedule: m[2], body: text.slice(m[0].length) }
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
// {value, label}. The list is drawn on the body, so no scrolling page or
// popover clips it, follows the button while open, and shuts when the
// button leaves the page. Keys follow a select-only combobox: arrows,
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
    document.body.append(list)
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

function convo(id) {
  if (!S.convos.has(id)) S.convos.set(id, { lastID: 0, entries: [], byItem: new Map() })
  return S.convos.get(id)
}

function apply(c, e) {
  if (e.id) {
    if (e.id <= c.lastID) return
    c.lastID = e.id
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
      // none is open: any other status means nothing is pending.
      if (e.status !== 'blocked') for (const en of c.entries) if (en.kind === 'approval' && !en.decision) en.decision = 'void'
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

const pending = (c) => c.entries.filter((en) => en.kind === 'approval' && !en.decision)
const decisionText = (d) => ({ accept: 'accepted', decline: 'declined', cancel: 'declined, turn stopped', void: 'the process ended' })[d] || d
const askText = (kind) => ({ command: 'run a command', file_change: 'change files', tool: 'use a tool', question: 'know' })[kind] || kind

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
    case 'code': return h('pre', { class: 'code' }, h('code', null, t.text))
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

// richText renders what a person writes lightly: fenced code, `inline
// code`, **bold**, paragraphs. A line of theirs that starts with "1." or
// "#" stays as typed. Text nodes only.
function richText(text) {
  const frag = document.createDocumentFragment()
  text.split('```').forEach((part, i) => {
    if (i % 2 === 1) {
      const nl = part.indexOf('\n')
      const body = nl >= 0 && /^[\w+.-]*$/.test(part.slice(0, nl)) ? part.slice(nl + 1) : part
      frag.append(h('pre', { class: 'code' }, h('code', null, body.replace(/\n$/, ''))))
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
  const es = new EventSource(`/v1/agents/${encodeURIComponent(id)}/events?follow=1&after=${c.lastID}`)
  es.onmessage = (m) => {
    const e = JSON.parse(m.data)
    apply(c, e)
    // Status rides the same stream; show it now rather than at the next poll.
    if (e.kind === 'status' && !e.ephemeral) {
      const a = agentById(id)
      if (a && a.state?.status !== e.status) {
        a.state = { ...a.state, status: e.status }
        renderSidebar()
        if (S.sel === id) renderTopbar()
      }
    }
    if (S.sel === id) scheduleConvo()
  }
  // After its history: an agent with none has none yet.
  es.addEventListener('ready', () => {
    c.ready = true
    if (S.sel === id) scheduleConvo()
  })
  es.onerror = () => {
    if (es.readyState === EventSource.CLOSED && S.sel === id) setTimeout(() => S.sel === id && follow(id), 2000)
  }
  S.es = es
  S.esFor = id
}

// Coalesce a burst of events into one render. A timer rather than
// requestAnimationFrame: rAF never fires in a hidden tab, and a tab brought
// back should show what happened while it was away.
let convoQueued = false
function scheduleConvo() {
  if (convoQueued) return
  convoQueued = true
  setTimeout(() => { convoQueued = false; renderConvo(); renderApprovals() }, 30)
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
}

async function refreshIsolations() {
  S.isolations = await api('GET', '/v1/isolations')
  if (S.view === 'isolations') {
    await refreshExposures()
    if (!editing('isolations')) renderIsolations()
  }
  if (S.view === 'new') S.refillIsolations?.()
  if (S.view === 'setup') {
    if (S.accounts == null) await refreshAccounts()
    // Reading a CLI starts it inside the isolation: once per visit, not on
    // every poll.
    const missing = S.isolations.map((i) => i.name).filter((n) => !(n in S.clis))
    if (missing.length) await refreshCLIs(missing)
  }
}

// editing is whether redrawing page id now would lose what the operator is
// typing: an inline form is open, or a field on it has the focus.
function editing(id) {
  if (S.openForm) return true
  const a = document.activeElement
  return !!(a && a.closest && a.closest('#' + id) && (/^(INPUT|SELECT|TEXTAREA)$/.test(a.tagName) || a.classList.contains('dropdown')))
}

async function refreshServices() {
  const [services, envs] = await Promise.all([api('GET', '/v1/services'), api('GET', '/v1/envs')])
  S.services = services || []
  S.envs = envs || []
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

function startPolling() {
  let n = 0
  const tick = async () => {
    // The first load always happens; after that a hidden tab stops polling
    // and catches up the moment it is shown again.
    if ((n === 0 || !document.hidden) && S.authed) {
      await guarded(refreshAgents)
      // The isolations list runs `container inspect` per isolation; keep it
      // to every 10s unless that page is open.
      if (S.view !== 'agent' || n % 5 === 0) await guarded(refreshIsolations)
      if (S.view === 'services') await guarded(refreshServices)
    }
    n++
    setTimeout(tick, 2000)
  }
  tick()
  document.addEventListener('visibilitychange', () => {
    if (!document.hidden && S.authed) { guarded(refreshAgents); guarded(refreshIsolations) }
  })
}

// ---- Actions

// The address bar mirrors the view (#/a/<id>, #/new, #/isolations), so a
// page can be bookmarked and a phone's back gesture does the expected thing.
function setHash(hash) {
  if (location.hash !== hash) history.pushState(null, '', hash)
}

function select(id, closeDrawer = true) {
  if (S.sel && S.sel !== id) S.drafts.set(S.sel, document.querySelector('.composer textarea')?.value || '')
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
  S.view = view
  S.drawer = false
  S.settingsOpen = false
  S.openForm = null
  setHash({ new: '#/new', isolations: '#/isolations', services: '#/services', setup: '#/setup' }[view] || location.hash)
  if (view === 'setup') { S.clis = {}; S.accounts = null }
  render()
  if (view !== 'agent') guarded(refreshIsolations)
  if (view === 'services') guarded(refreshServices)
}

function route() {
  const m = location.hash.match(/^#\/a\/([a-z0-9-]+)$/)
  if (m) { S.sel = m[1]; S.view = 'agent'; follow(m[1]) }
  else if (location.hash === '#/new') S.view = 'new'
  else if (location.hash === '#/isolations') S.view = 'isolations'
  else if (location.hash === '#/services') { S.view = 'services'; guarded(refreshServices) }
  else if (location.hash === '#/setup') { S.view = 'setup'; S.clis = {}; S.accounts = null; guarded(refreshIsolations) }
  else S.view = 'agent'
  S.drawer = false
  S.settingsOpen = false
  render()
}
window.addEventListener('popstate', route)

async function send(text) {
  const id = S.sel
  await guarded(async () => {
    // One of gero's own commands is done, not sent, and says what it did.
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
try { const w = +localStorage.getItem('gero.sidebarWidth'); if (w) setSidebarWidth(w) } catch {}

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
    try { if (w) localStorage.setItem('gero.sidebarWidth', String(w)) } catch {}
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
    try { localStorage.removeItem('gero.sidebarWidth') } catch {}
    fitTopbar()
  })
  return bar
}

// ---- Rendering

function render() {
  root.className = 'app' + (S.drawer ? ' drawer' : '')
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
  if (S.view === 'new') main.append(topbar('New agent'), newAgentView())
  else if (S.view === 'isolations') main.append(topbar('Isolations'), h('div', { class: 'page', id: 'isolations' }))
  else if (S.view === 'services') main.append(topbar('Services'), h('div', { class: 'page', id: 'services' }))
  else if (S.view === 'setup') main.append(topbar('Accounts'), h('div', { class: 'page', id: 'setup' }))
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

function renderSidebar() {
  const el = document.getElementById('sidebar')
  if (!el) return
  if (S.sorting) { S.sidebarStale = true; return }
  S.sidebarStale = false
  const live = S.agents.filter((a) => !a.archived_at)
  const archived = S.agents.filter((a) => a.archived_at)
  const row = (a) => {
    const live = a.archived_at ? 'idle' : a.state?.status || 'idle'
    const st = live === 'idle' && a.parked ? 'parked' : live
    // An idle agent's dot parks it, a parked one's takes it back: it is
    // idle and wants a look again, as it does once it next does anything.
    const togglable = !a.archived_at && live === 'idle'
    const dot = h('span', {
      class: 'dot ' + st + (togglable ? ' toggle' : ''), role: togglable ? 'button' : 'img', 'aria-label': st,
      title: !togglable ? st : st === 'parked' ? 'parked: needs no look; tap to make it idle' : 'idle: wants a look; tap to park it',
    })
    if (togglable) {
      dot.addEventListener('click', async (e) => {
        e.stopPropagation()
        const want = !a.parked
        a.parked = want
        renderSidebar()
        await guarded(async () => {
          try {
            await api('PATCH', `/v1/agents/${a.id}`, { parked: want })
          } catch (x) {
            a.parked = !want
            renderSidebar()
            throw x
          }
        })
      })
    }
    const meta = a.problem ? h('span', { class: 'problem' }, icon('triangle-alert'), ' ' + a.problem)
      : [`${shownID(a)} · ${a.isolation}`, a.inbox ? [' · ', icon('mail'), ` ${a.inbox}`] : null]
    return h('button', {
      class: 'agent' + (a.id === S.sel && S.view === 'agent' ? ' selected' : '') + (a.archived_at ? ' archived' : ''),
      'data-line': a.line || a.id, onclick: () => select(a.id),
    }, dot, h('span', { class: 'name' }, agentTitle(a)), h('span', { class: 'meta' }, meta))
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
  el.replaceChildren(
    h('div', { class: 'brand' }, 'gero', h('span', { class: 'count' }, `${live.length} agent${live.length === 1 ? '' : 's'}`),
      h('button', { class: 'add-divider', title: 'Add a divider at the bottom; hold it to move it', 'aria-label': 'Add a divider', onclick: addDivider },
        icon('separator-horizontal'))),
    h('button', { class: 'new-agent' + (S.view === 'new' ? ' active' : ''), onclick: () => show('new') }, '+ New agent'),
    h('div', { class: 'agent-list' },
      sortable('live', liveRows(live, row)),
      h('button', { class: 'fold', onclick: () => { S.showArchived = !S.showArchived; renderSidebar() } },
        icon(S.showArchived ? 'chevron-down' : 'chevron-right'), ` Archived (${archived.length})`),
      S.showArchived ? sortable('archived', archivedRows()) : null),
    h('div', { class: 'side-foot' },
      h('button', { class: S.view === 'isolations' ? 'active' : '', onclick: () => show('isolations') }, icon('box'), ' Isolations'),
      h('button', { class: S.view === 'services' ? 'active' : '', onclick: () => show('services') }, icon('key-round'), ' Services'),
      h('button', { class: S.view === 'setup' ? 'active' : '', onclick: () => show('setup') }, icon('user-round'), ' Accounts')),
  )
}

// sortable is a sidebar list whose rows, a line each, move by a long press
// (a long click with a mouse) and a drag, within the list only. The order
// is kept by the daemon, so every page has it.
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
    if (e.button !== 0 || box.children.length < 2) return
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

// liveRows are the live agents' rows with the dividers where the order
// has them; agents it has not placed come after, as the daemon sorts them.
function liveRows(live, row) {
  const byLine = new Map(live.map((a) => [a.line || a.id, a]))
  const out = [], drawn = new Set()
  for (const l of S.order?.live || []) {
    if (isDivider(l)) out.push(dividerRow(l))
    else if (byLine.has(l)) { out.push(row(byLine.get(l))); drawn.add(l) }
  }
  for (const a of live) if (!drawn.has(a.line || a.id)) out.push(row(a))
  return out
}

function dividerRow(line) {
  return h('div', { class: 'divider', 'data-line': line, role: 'separator' },
    h('button', {
      class: 'divider-x', title: 'Remove this divider', 'aria-label': 'Remove divider',
      onclick: (e) => { e.stopPropagation(); setLiveOrder(liveLines().filter((l) => l !== line)) },
    }, icon('x')))
}

// liveLines are the live list as it is drawn, from the top.
function liveLines() {
  const box = document.querySelector('#sidebar .sortable')
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
  const inList = (a) => !!a.archived_at === (list === 'archived')
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
    frag.append(topbar('gero'), h('div', { class: 'page' }, h('div', { class: 'empty' },
      h('p', null, S.agents.length ? 'No agent selected.' : 'No agents yet.'),
      h('button', { class: 'btn primary', onclick: () => show('new') }, 'Start an agent'))))
    return frag
  }
  S.drawn = a.id
  const frag = document.createDocumentFragment()
  frag.append(
    h('div', { id: 'topbar-slot' }),
    h('div', { class: 'convo', id: 'convo' }, h('div', { class: 'convo-inner', id: 'convo-inner' })),
    h('div', { id: 'approvals' }),
    composer(a),
  )
  queueMicrotask(() => { renderTopbar(); renderConvo(true); renderApprovals() })
  return frag
}

// forkedFrom is the ID of the agent a was forked from, while gero has it.
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
    topbar(agentTitle(a), `${a.kind} · ${a.isolation} · ${shownID(a)}` + (forkedFrom(a) ? ` · fork of ${forkedFrom(a)}` : ''),
      h('span', { class: 'chip ' + st }, st),
      h('div', { class: 'actions' },
        working ? h('button', { class: 'btn stop', onclick: () => guarded(() => api('POST', `/v1/agents/${a.id}/interrupt`)) }, icon('square'), h('span', { class: 'label' }, ' Stop')) : null,
        h('button', { class: 'btn ghost', title: 'Its mail: what it was sent and what it wrote', onclick: () => openMail(a) },
          icon('mail'), h('span', { class: 'label' }, ' Mail'), a.inbox ? h('span', { class: 'badge' }, String(a.inbox)) : null),
        a.archived_at ? null : h('button', { class: 'btn ghost', title: 'Cron: messages sent to it on a schedule', onclick: () => openCrons(a) },
          icon('clock'), h('span', { class: 'label' }, ' Cron'), a.crons ? h('span', { class: 'badge' }, String(a.crons)) : null),
        a.archived_at ? null : h('button', { class: 'btn ghost', title: 'TODO: prompts put aside to send it later', onclick: () => openTodos(a) },
          icon('list-todo'), h('span', { class: 'label' }, ' TODO'), a.todos ? h('span', { class: 'badge' }, String(a.todos)) : null),
        h('button', { class: 'btn ghost', title: 'Name and ID', onclick: () => editAgent(a) }, icon('pencil'), h('span', { class: 'label' }, ' Name & ID')),
        h('button', {
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
        a.archived_at ? null : h('button', {
          class: 'btn ghost', title: 'Archive', onclick: async () => {
            if (!await ask({ title: `Archive "${agentTitle(a)}"?`, body: 'Its history is kept.', ok: 'Archive' })) return
            await guarded(() => api('POST', `/v1/agents/${a.id}/archive`))
            S.sel = null
            await guarded(refreshAgents)
            render()
          },
        }, icon('archive'), h('span', { class: 'label' }, ' Archive')))),
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

// editAgent changes what an agent is called: its name, and its own ID,
// which the daemon checks against every agent's.
async function editAgent(a) {
  const name = h('input', { class: 'dialog-input', value: a.name, autocomplete: 'off', placeholder: 'from its first message' })
  const id = h('input', { class: 'dialog-input', value: a.handle || '', autocomplete: 'off', autocapitalize: 'off', spellcheck: 'false', placeholder: a.id })
  let saved = null
  const ok = await ask({
    title: 'Name and ID',
    body: `Only what it is called changes; a running turn carries on. Its ID is unique among all agents; left empty, it goes by ${a.id}, the one gero gave it.`,
    fields: [h('label', { class: 'dialog-field' }, 'Name', name), h('label', { class: 'dialog-field' }, 'ID', id)],
    ok: 'Save',
    onOk: async () => {
      const body = {}
      const n = name.value.trim()
      const i = id.value.trim()
      if (n !== a.name) {
        if (!n) throw new Error('A name is required.')
        body.name = n
      }
      if (i !== (a.handle || '')) body.handle = i
      if (Object.keys(body).length) saved = await api('PATCH', `/v1/agents/${enc(a.id)}`, body)
    },
  })
  if (!ok || !saved) return
  toast(`"${agentTitle(saved)}" goes by ${shownID(saved)}`)
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
      c.waiting_since ? h('span', { class: 'chip waiting', title: `since ${when(c.waiting_since)}` }, 'waiting for idle') : null),
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
    S.drafts.set(a.id, next)
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
    const body = { schedule: spec, prompt: prompt.value, if_busy: wait.querySelector('input').checked ? 'wait' : 'skip' }
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
      h('span', { class: 'hint' }, `The Mac's time: ${zone}. Runs missed while the Mac slept or gero was down come due once, as soon as they can.`)),
    h('label', { class: 'dialog-field' }, 'Message', prompt),
    h('div', { class: 'dialog-field' }, 'If the agent is busy when it comes', h('div', { class: 'busy' }, skip, wait)),
    err,
    h('div', { class: 'dialog-actions' }, h('button', { type: 'button', class: 'btn', onclick: cancel }, 'Back'), save))
  form.addEventListener('keydown', (e) => {
    if (e.key === 'Enter' && e.target.matches('input') && !e.isComposing) { e.preventDefault(); if (!save.disabled) go() }
  })
  queueMicrotask(() => (c.id ? prompt : kind === 'cron' ? expr : n).focus())
  return form
}

function renderConvo(forceBottom) {
  const box = document.getElementById('convo')
  const inner = document.getElementById('convo-inner')
  const a = agentById(S.sel)
  if (!box || !inner || !a) return
  const atBottom = forceBottom || box.scrollHeight - box.scrollTop - box.clientHeight < 80
  const c = convo(a.id)
  // The page is redrawn as events arrive; a message being edited keeps its
  // caret through that.
  const typing = document.activeElement?.classList.contains('edit-box') ? document.activeElement : null
  const sel = typing && [typing.selectionStart, typing.selectionEnd]
  const items = groupTools(c.entries).map((en) => entryView(en, a)).filter(Boolean)
  const none = c.ready ? 'Nothing yet: the first message you send starts it.' : 'Loading…'
  inner.replaceChildren(...(items.length ? items : [h('div', { class: 'empty' }, none)]))
  const ta = typing && inner.querySelector('.edit-box')
  if (ta) { ta.focus({ preventScroll: true }); ta.setSelectionRange(...sel) }
  if (atBottom) box.scrollTop = box.scrollHeight
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
      const cm = cronMsg(en.text)
      if (cm) return h('div', { class: 'msg-cron' }, h('div', { class: 'who' }, icon('clock'), ` Cron #${cm.id} · ${describeSchedule(cm.schedule)}`), h('div', { class: 'text' }, richText(cm.body)))
      if (!frozen && S.edit?.agent === a.id && S.edit.id === en.id) return editView(en, a)
      const bubble = h('div', { class: 'msg-user text' }, richText(en.text))
      const edit = !frozen && en.id && rewindable(a)
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
      return h('details', { class: 'tool' },
        h('summary', null, h('span', { class: 'mark ' + en.state }, mark), h('span', { class: 'tname' }, en.name), h('span', { class: 'tsum' }, en.summary)),
        out ? h('pre', null, clip(out, 40)) : null)
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

function renderApprovals() {
  const box = document.getElementById('approvals')
  const a = agentById(S.sel)
  if (!box || !a) return
  const p = pending(convo(a.id))
  if (!p.length) { box.replaceChildren(); return }
  const ap = p[0]
  if (ap.questions) { renderQuestion(box, a, ap, p.length); return }
  box.replaceChildren(h('div', { class: 'composer pending-approval' },
    h('div', { class: 'composer-row' }, h('div', { class: 'approval grow' },
      h('div', { class: 'head' }, `${label(a)} asks to ${askText(ap.akind)}` + (p.length > 1 ? ` (+${p.length - 1} more)` : '')),
      h('div', { class: 'what' }, ap.summary),
      ap.reason ? h('div', { class: 'why' }, ap.reason) : null,
      h('div', { class: 'actions' },
        h('button', { class: 'btn primary', onclick: () => decide(ap.id, 'accept') }, 'Accept'),
        h('button', { class: 'btn', onclick: () => decide(ap.id, 'decline') }, 'Decline'),
        h('button', { class: 'btn danger', onclick: () => decide(ap.id, 'cancel') }, 'Decline & stop'))))))
}

// renderQuestion is a question the agent asks (Claude's AskUserQuestion):
// each of its questions with its options to pick, one or several, and a
// box for an answer of one's own. What is picked lives in S, since the page
// is redrawn as events arrive; a box being typed in keeps its caret.
function renderQuestion(box, a, ap, n) {
  if (S.answering?.id !== ap.id) S.answering = { id: ap.id, picked: {}, own: {} }
  const st = S.answering
  const typing = document.activeElement?.dataset?.q != null ? document.activeElement : null
  const sel = typing && [typing.dataset.q, typing.selectionStart, typing.selectionEnd]
  const answerTo = (q) => {
    const own = (st.own[q.question] || '').trim()
    if (!q.multi_select) return own || st.picked[q.question] || ''
    return [...(st.picked[q.question] || []), ...(own ? [own] : [])].join(', ')
  }
  const answer = h('button', { class: 'btn primary' }, 'Answer')
  const ready = () => { answer.disabled = !ap.questions.every((q) => answerTo(q)) }
  answer.addEventListener('click', () => {
    const answers = Object.fromEntries(ap.questions.map((q) => [q.question, answerTo(q)]))
    decide(ap.id, 'accept', answers)
  })
  const question = (q, i) => {
    const picked = st.picked[q.question]
    const opts = (q.options || []).map((o) => {
      const on = q.multi_select ? (picked || []).includes(o.label) : picked === o.label && !(st.own[q.question] || '').trim()
      return h('button', {
        class: 'btn small opt' + (on ? ' on' : ''), title: o.description || '', 'aria-pressed': String(on),
        onclick: () => {
          if (!q.multi_select) { st.picked[q.question] = o.label; st.own[q.question] = '' }
          else st.picked[q.question] = on ? picked.filter((l) => l !== o.label) : [...(picked || []), o.label]
          renderApprovals()
        },
      }, o.label)
    })
    const own = h('input', { class: 'own', value: st.own[q.question] || '', placeholder: q.multi_select ? 'And in your own words…' : 'Or in your own words…', autocomplete: 'off' })
    own.dataset.q = String(i)
    own.addEventListener('input', () => { st.own[q.question] = own.value; ready() })
    own.addEventListener('keydown', (e) => { if (e.key === 'Enter' && !e.isComposing && !answer.disabled) answer.click() })
    return h('div', { class: 'q' },
      h('div', { class: 'q-head' }, q.header ? h('span', { class: 'chip' }, q.header) : null, q.question,
        q.multi_select ? h('span', { class: 'hint' }, ' (any of them)') : null),
      opts.length ? h('div', { class: 'opts' }, opts) : null, own)
  }
  box.replaceChildren(h('div', { class: 'composer pending-approval' },
    h('div', { class: 'composer-row' }, h('div', { class: 'approval grow' },
      h('div', { class: 'head' }, `${label(a)} asks you` + (n > 1 ? ` (+${n - 1} more)` : '')),
      ap.questions.map(question),
      h('div', { class: 'actions' },
        answer,
        h('button', { class: 'btn', onclick: () => decide(ap.id, 'decline') }, 'Decline'),
        h('button', { class: 'btn danger', onclick: () => decide(ap.id, 'cancel') }, 'Decline & stop'))))))
  ready()
  if (sel) {
    const el = box.querySelector(`input[data-q="${sel[0]}"]`)
    if (el) { el.focus({ preventScroll: true }); el.setSelectionRange(sel[1], sel[2]) }
  }
}

// ---- Completion in the message box: commands typed at the start of a
// message (/; gero's own, then the agent CLI's), Codex's skills anywhere in
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
        it.gero ? h('span', { class: 'ci-tag', title: 'gero does this itself; the agent is not sent it' }, 'gero') : null)),
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
      // Typed: the best match first; not yet: gero's own, then the agent's.
      .sort((x, y) => (t.q ? y[0] - x[0] : 0))
      .map(([, x]) => ({ label: x.prefix + x.name, value: x.prefix + x.name, hint: x.hint, desc: x.description, gero: x.gero,
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
    S.drafts.set(a.id, '')
    grow()
    await send(text)
    // Its first message names it; show that now, not at the next poll.
    if (!agentById(a.id)?.name) await guarded(refreshAgents)
  }
  ta.addEventListener('input', grow)
  const done = completer(a, ta, submit)
  ta.addEventListener('keydown', (e) => {
    if (done.key(e)) return
    if (e.key === 'Enter' && !e.shiftKey && !coarse && !e.isComposing) { e.preventDefault(); submit() }
    if (e.key === 'Escape') { S.settingsOpen = false; renderSettings(a) }
  })
  queueMicrotask(grow)
  const wrap = h('div', { class: 'composer' },
    h('div', { id: 'settings-slot' }),
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
  const todo = () => guarded(async () => {
    close()
    const text = ta.value.trim()
    if (!text) { ta.focus(); return }
    await api('POST', `/v1/agents/${enc(a.id)}/todos`, { text })
    ta.value = ''
    S.drafts.set(a.id, '')
    ta.dispatchEvent(new Event('input'))
    toast('Put aside in TODO')
    guarded(refreshAgents)
  })
  menu.append(h('button', { type: 'button', class: 'btn primary send-btn', onclick: todo }, 'TODO'))
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

// planLimits are a's plan windows as its isolation's agents last said
// them: they are the account's, not its own, and an agent that has not
// run since a window started over says the one before.
function planLimits(a) {
  const latest = new Map()
  for (const b of S.agents) {
    if (b.archived_at || b.isolation !== a.isolation) continue
    for (const l of b.state?.usage?.limits || []) {
      // Within one window use only grows: the most said is the latest.
      const k = latest.get(l.window), at = l.resets_at || 0, kat = k?.resets_at || 0
      if (!k || kat < at || kat === at && k.percent < l.percent) latest.set(l.window, l)
    }
  }
  const now = Date.now() / 1000
  return [...latest.values()].filter((l) => !l.resets_at || l.resets_at > now).sort((p, q) => p.window.localeCompare(q.window))
}

function renderStatusLine() {
  const slot = document.getElementById('status-slot')
  const a = agentById(S.sel)
  if (!slot || !a) return
  const u = a.state?.usage
  if (!u || a.archived_at) { slot.replaceChildren(); return }
  const dir = (u.cwd || a.workdir || '').replace(/^\/home\/agent(?=\/|$)/, '~')
  const branch = readBranch(a)
  const parts = [
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
    h('div', { class: 'note' }, a.kind === 'claude' ? 'Claude restarts with these when it is idle.' : 'Codex uses these from the next turn.'),
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
  try { return JSON.parse(localStorage.getItem('gero.recentDirs') || '{}')[iso] || null } catch { return null }
}

function rememberDir(iso, dir) {
  try {
    const all = JSON.parse(localStorage.getItem('gero.recentDirs') || '{}')
    all[iso] = dir
    localStorage.setItem('gero.recentDirs', JSON.stringify(all))
  } catch {}
}

function dirCompleter(input, isoName) {
  const box = h('div', { class: 'complete below', role: 'listbox', hidden: true })
  let items = [], active = 0, seq = 0, timer = null
  const close = () => { box.hidden = true; items = []; seq++ }
  let recent = null
  const draw = (note) => {
    box.replaceChildren(...items.map((d, i) => h('div', {
      class: 'complete-item dir' + (i === active ? ' active' : ''), role: 'option', 'aria-selected': String(i === active),
      onmousedown: (e) => { e.preventDefault(); active = i; accept() },
    }, h('span', { class: 'ci-name' }, d),
      d === recent ? h('span', { class: 'ci-tag', title: 'The directory last given for an agent in this isolation' }, 'recent') : null)),
    ...(note ? [h('div', { class: 'complete-note' }, note)] : []))
    box.hidden = !items.length && !note
    box.querySelector('.active')?.scrollIntoView({ block: 'nearest' })
  }
  const ask = async () => {
    const iso = isoName()
    if (!iso || document.activeElement !== input) return close()
    const my = ++seq
    const typed = input.value
    try {
      const d = await api('GET', `/v1/isolations/${encodeURIComponent(iso)}/dirs?path=${encodeURIComponent(typed)}`)
      if (my !== seq) return
      const tilde = (p) => (typed.trim() === '' || typed.trim().startsWith('~')) && (p + '/').startsWith(d.home + '/') ? '~' + p.slice(d.home.length) : p
      items = d.dirs.map(tilde)
      // The last one given in this isolation leads, while what is typed
      // could still become it.
      recent = recentDir(iso)
      const t = typed.trim()
      if (recent && recent !== t && recent.startsWith(t)) items = [recent, ...items.filter((p) => p !== recent)]
      else recent = null
      active = 0
      draw(items.length ? null : 'No such directory here: it is made when the agent is.')
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
        close()
    }
  })
  return h('div', { class: 'with-complete' }, input, box)
}

function newAgentView() {
  let kind = 'claude'
  const err = h('p', { class: 'form-error' })
  const iso = dropdown([], { label: 'Isolation' })
  const noIso = h('p', { class: 'form-error' }, 'There is no isolation yet. ',
    h('a', { href: '#/isolations', onclick: (e) => { e.preventDefault(); show('isolations') } }, 'Create one'), ' first.')
  // The list may arrive after the form is drawn; refill it in place so
  // what is already chosen survives.
  const fillIsos = () => {
    iso.setOptions(S.isolations.map((i) => i.name))
    noIso.hidden = S.isolations.length > 0
  }
  fillIsos()
  S.refillIsolations = fillIsos
  const handle = h('input', { placeholder: 'optional; like brave-otter otherwise', autocomplete: 'off', autocapitalize: 'off', spellcheck: 'false' })
  const workdir = h('input', { placeholder: 'optional; ~/work/<its ID> otherwise', autocomplete: 'off', autocapitalize: 'off', spellcheck: 'false' })
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
        workdir: workdir.value.trim() || undefined,
        options: { effort: effort.value || undefined, model: model.value.trim() || undefined, approvals: approvals.value },
      })
      if (workdir.value.trim()) rememberDir(iso.value, workdir.value.trim())
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

function renderIsolations() {
  const page = document.getElementById('isolations')
  if (!page) return
  const fenceText = (i) => i.fence_state === 'verified' ? 'network fence verified' : i.fence_state === 'off' ? 'network fence off' : null
  const card = (i) => {
    const key = h('input', { placeholder: 'VARIABLE', autocomplete: 'off' })
    const val = h('input', { type: 'password', placeholder: 'value (empty removes it)', autocomplete: 'new-password' })
    return h('div', { class: 'card' },
      h('div', { class: 'head' },
        h('h3', null, i.name),
        h('span', { class: 'chip ' + (i.state === 'running' ? '' : 'blocked') }, i.state),
        h('span', { class: 'chip ' + (i.fence_state === 'verified' ? '' : i.fence_state === 'off' ? 'blocked' : 'error') }, fenceText(i) || ['fence ', icon('x')])),
      i.fence_state !== 'verified' && i.fence_state !== 'off' ? h('div', { class: 'fence-bad' }, i.fence_state) : null,
      h('div', { class: 'facts' }, `${i.image} · ${isoSize(i.cpus, i.memory)} · network ${i.network || 'default'} · env: ${(i.env_keys || []).join(', ') || '—'}`),
      S.openForm === `verify:${i.name}` && S.verifyOut ? verifyView(S.verifyOut) : null,
      exposuresView(i),
      h('div', { class: 'inline-form' }, key, val,
        h('button', {
          class: 'btn', onclick: async () => {
            const k = key.value.trim()
            if (!k) return
            await guarded(async () => {
              await api('PATCH', `/v1/isolations/${i.name}/env`, { [k]: val.value === '' ? null : val.value })
              toast(`${k} saved; idle agents restart with it`)
              await refreshIsolations()
            })
          },
        }, 'Set')),
      h('div', { class: 'actions' },
        h('button', { class: 'btn', disabled: S.verifying === i.name, onclick: () => verifyIsolation(i) },
          S.verifying === i.name ? 'Measuring…' : 'Verify'),
        h('button', { class: 'btn', onclick: () => openForm(`settings:${i.name}`) }, 'Settings'),
        h('button', {
          class: 'btn', onclick: async () => {
            if (!await ask({ title: `Upgrade ${i.name} to the current image?`, body: 'Running agents restart; sessions are kept.', ok: 'Upgrade' })) return
            toast('Upgrading…')
            await guarded(async () => { await api('POST', `/v1/isolations/${i.name}/upgrade`); toast(`${i.name} upgraded`); await refreshIsolations() })
          },
        }, 'Upgrade'),
        h('button', {
          class: 'btn danger', onclick: async () => {
            if (!await ask({ title: `Delete isolation ${i.name}?`, body: 'Its volumes are kept.', ok: 'Delete', danger: true })) return
            await guarded(async () => { await api('DELETE', `/v1/isolations/${i.name}`); toast(`${i.name} deleted`); await refreshIsolations() })
          },
        }, 'Delete')),
      S.openForm === `settings:${i.name}` ? isoSettingsForm(i) : null,
      h('p', { class: 'note' }, 'What its Claude and Codex log in with: ',
        h('a', { href: '#/setup', onclick: (e) => { e.preventDefault(); show('setup') } }, 'Accounts'), '.'))
  }
  // The new isolation lives in S, typed and while it is created, since
  // polling redraws this page: drawn from the DOM it came back empty two
  // seconds after Create, as if nothing had happened.
  const d = S.isoDraft
  const busy = !!S.isoCreating
  const name = h('input', { placeholder: 'name', autocomplete: 'off', value: d.name, disabled: busy })
  const memory = h('input', { value: d.memory, disabled: busy })
  const fence = h('input', { type: 'checkbox', checked: d.fence, disabled: busy })
  name.addEventListener('input', () => { d.name = name.value })
  memory.addEventListener('input', () => { d.memory = memory.value })
  fence.addEventListener('change', () => { d.fence = fence.checked })
  const create = h('button', { class: 'btn primary', disabled: busy },
    busy ? `Creating ${S.isoCreating}… (building its image first when gero has changed)` : 'Create isolation')
  create.addEventListener('click', async () => {
    const n = d.name.trim()
    if (!n) return
    S.isoCreating = n
    S.isoCreateError = null
    renderIsolations()
    try {
      await api('POST', '/v1/isolations', { name: n, memory: d.memory.trim(), fence: d.fence ? 'on' : 'off' })
      S.isoDraft = { name: '', memory: '4G', fence: true }
      toast(`Isolation ${n} created`)
    } catch (x) {
      if (x instanceof Unauthorized) { S.authed = false; render(); return }
      S.isoCreateError = x.message
      if (NO_KERNEL.test(x.message)) noKernel()
    } finally {
      S.isoCreating = null
    }
    await guarded(refreshIsolations)
    renderIsolations()
  })
  page.replaceChildren(h('div', { class: 'page-inner' },
    h('h2', null, 'Isolations'),
    h('p', { class: 'lead' }, 'Each runs in its own VM, fenced off the Mac and the LAN, with internet access.'),
    S.isolations.map(card),
    h('div', { class: 'card' },
      h('div', { class: 'head' }, h('h3', null, 'New isolation')),
      h('div', { class: 'row spaced' },
        h('div', { class: 'field' }, h('label', null, 'Name'), name),
        h('div', { class: 'field' }, h('label', null, 'Memory'), memory)),
      h('label', { class: 'check' }, fence, 'Network fence (agents start only behind a verified fence)'),
      S.isoCreateError ? h('p', { class: 'form-error' }, S.isoCreateError) : null,
      h('div', { class: 'actions' }, create))))
}

// NO_KERNEL is container refusing to start a VM before a Linux kernel is
// set up, which a fresh install of container is: nothing in gero fixes it,
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

// isoSize is a VM's CPUs and memory; none set is container's default.
function isoSize(cpus, memory) {
  return `${cpus ? cpus + ' CPUs' : 'default CPUs'}, ${memory || 'default memory'}`
}

// isoSettingsForm renames an isolation or resizes its VM. A new name
// changes nothing in the VM and goes straight through; a new size replaces
// the container, so it asks first.
function isoSettingsForm(i) {
  const name = h('input', { value: i.name, autocomplete: 'off' })
  const cpus = h('input', { value: i.cpus ? String(i.cpus) : '', inputmode: 'numeric', placeholder: 'default', autocomplete: 'off' })
  const memory = h('input', { value: i.memory || '', placeholder: 'default', autocomplete: 'off' })
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
    if (!Object.keys(body).length) { closeForm(); return }
    const resize = 'cpus' in body || 'memory' in body
    if (resize && !await ask({
      title: `Resize ${i.name} to ${isoSize(c, m)}?`,
      body: 'The container is replaced: running agents restart. Sessions and workspaces are kept.',
      ok: 'Resize',
    })) return
    save.disabled = true
    save.textContent = resize ? 'Resizing…' : 'Saving…'
    try {
      const v = await api('PATCH', `/v1/isolations/${enc(i.name)}`, body)
      S.openForm = null
      toast(!resize ? `${i.name} is now ${v.name}; its agents kept running`
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
      h('div', { class: 'field' }, h('label', null, 'Memory'), memory, h('span', { class: 'hint' }, 'Like 8G. A new size replaces the container.'))),
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

function compactConfig(cfg) {
  return Object.keys(cfg || {}).sort().map((k) => `${k}=${Array.isArray(cfg[k]) ? cfg[k].join(',') : cfg[k]}`).join(' ')
}

function renderServices() {
  const page = document.getElementById('services')
  if (!page) return
  page.replaceChildren(h('div', { class: 'page-inner' },
    h('h2', null, 'Services'),
    h('p', { class: 'lead' }, 'Credentials stay on the Mac; isolations get proxies that use them within an allowlist. Define an environment here, then turn it on for an isolation on the Isolations page.'),
    S.services.length ? S.services.map(serviceCard) : h('div', { class: 'empty' }, 'Loading…')))
}

function serviceCard(s) {
  const envs = S.envs.filter((e) => e.service === s.name)
  const key = 'new:' + s.name
  return h('div', { class: 'card' },
    h('div', { class: 'head' }, h('h3', null, s.name), h('span', { class: 'note' }, s.summary)),
    envs.map((e) => envRow(s, e)),
    // A service with nothing to define (mail) is only turned on.
    !s.fields?.length ? h('p', { class: 'note' }, 'Nothing to define here: turn it on for an isolation on the Isolations page.')
      : S.openForm === key ? envForm(s, null) : h('div', { class: 'actions' }, h('button', { class: 'btn', onclick: () => openForm(key) }, '+ New environment')))
}

function secretChip(s, e) {
  if (!s.secrets?.length) return null
  if (e.secret_error) return h('span', { class: 'chip error' }, 'secret unreadable')
  if (e.credential_elsewhere) return h('span', { class: 'chip' }, 'token command')
  if (e.missing_secrets?.length) return h('span', { class: 'chip blocked' }, s.login ? 'not logged in' : 'no secret')
  return h('span', { class: 'chip' }, s.login ? 'logged in' : 'secret set')
}

function envRow(s, e) {
  const k = (what) => `${what}:${s.name}:${e.name}`
  let open = null
  if (S.openForm === k('edit')) open = envForm(s, e)
  else if (S.openForm === k('secret')) open = secretForm(s, e)
  else if (S.openForm === k('login') && S.login) open = loginForm(s, e)
  else if (S.openForm === k('check') && S.checkOut) {
    open = h('div', { class: 'out' + (S.checkOut.error ? ' bad' : '') },
      h('pre', null, (S.checkOut.output || '') + (S.checkOut.error ? '✗ ' + S.checkOut.error : '')),
      h('button', { class: 'btn', onclick: closeForm }, 'Close'))
  }
  const exposed = e.exposed_to?.length ? 'on for ' + e.exposed_to.join(', ') : 'not on for any isolation'
  return h('div', { class: 'env' },
    h('div', { class: 'head' }, h('strong', null, e.name), secretChip(s, e), h('span', { class: 'note' }, exposed)),
    h('div', { class: 'facts' }, compactConfig(e.config) || '—'),
    open || h('div', { class: 'actions' },
      h('button', { class: 'btn', onclick: () => openForm(k('edit')) }, 'Edit'),
      s.secrets?.length ? h('button', {
        class: 'btn', onclick: () => s.login ? startLogin(s, e, k('login')) : openForm(k('secret')),
      }, s.login ? 'Log in' : 'Secret') : null,
      h('button', { class: 'btn', onclick: () => checkEnv(s, e, k('check')) }, 'Check'),
      h('button', {
        class: 'btn danger', onclick: async () => {
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

function envForm(s, e) {
  const name = e ? null : h('input', { placeholder: 'letters, digits, - and _', autocomplete: 'off' })
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
      env: e ? e.name : undefined,
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
    const n = e ? e.name : name.value.trim()
    if (!n) { err.textContent = 'A name is required.'; return }
    if (s.replaces && e?.exposed_to?.length && !await ask({
      title: `Restart ${e.exposed_to.join(', ')} with it?`,
      body: 'A changed folder is mounted only when the VM starts, so the container of each isolation that has it is replaced: running agents end and start again on their next message.',
      ok: 'Save and restart',
    })) return
    const secret = Object.fromEntries(secrets.map(({ f, el }) => [f, el.value.trim()]))
    try {
      const v = await api(e ? 'PATCH' : 'POST', envPath(s.name, n), ctl.patch())
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
    name ? h('div', { class: 'field' }, h('label', null, 'name'), name, h('span', { class: 'hint' }, 'what isolations expose')) : null,
    secrets.map(({ f, el }) => h('div', { class: 'field' }, h('label', null, f), el,
      h('span', { class: 'hint' }, 'Stored in the Mac’s credentials file (0600), never shown again. The proxy uses it; the isolation never sees it.'))),
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
    h('p', { class: 'note' }, 'Written to the Mac’s credentials file (0600) and never shown again.'), err,
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

async function checkEnv(s, e, key) {
  toast(`Checking ${s.name}/${e.name}…`)
  await guarded(async () => {
    S.checkOut = await api('POST', envPath(s.name, e.name) + '/check')
    S.toast = null
    renderToast()
    openForm(key)
  })
}

// ---- Verify: an isolation's guarantees, measured now (gero iso verify)

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

function exposuresView(i) {
  const xs = S.exposures[i.name]
  if (!xs || !S.services.length) return null
  const byName = Object.fromEntries(xs.map((x) => [x.service, x]))
  return h('div', { class: 'svc-list' },
    h('div', { class: 'svc-title' }, 'Services', h('span', { class: 'note' }, ' — the credentials stay on the Mac')),
    S.services.map((s) => {
      const x = byName[s.name] || { service: s.name, enabled: false, envs: [] }
      const key = `expose:${i.name}:${s.name}`
      const detail = !x.enabled ? 'off'
        : !s.fields?.length ? compactConfig(x.settings) || 'on'
          : s.replaces ? (x.envs || []).map((n) => '/shared/' + n).join(', ') || 'no folder'
            : s.forwards ? (x.forwards?.length ? forwardLinks(x.forwards) : 'no port')
              : [(x.envs || []).join(', ') || 'no env', x.default_env ? `(default ${x.default_env})` : '', compactConfig(x.settings)].filter(Boolean).join(' ')
      return h('div', { class: 'svc-row' },
        h('div', { class: 'svc-line' },
          h('span', { class: 'dot' + (x.enabled ? ' on' : '') }),
          h('strong', null, s.name), h('span', { class: 'facts' }, detail),
          x.missing?.length ? h('span', { class: 'chip error' }, 'missing ' + x.missing.join(',')) : null,
          (x.forwards || []).filter((f) => f.error).map((f) => h('span', { class: 'chip error', title: f.error }, `${f.env} not listening`)),
          S.openForm === key ? null : h('button', { class: 'btn small', onclick: () => openForm(key) }, x.enabled ? 'Change' : 'Turn on')),
        S.openForm === key ? exposeForm(i, s, x) : null)
    }))
}

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
  const boxes = names.map((n) => ({ n, el: h('input', { type: 'checkbox', checked: have.has(n) }) }))
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
      toast(s.replaces ? `${i.name} has ${envs.map((n) => '/shared/' + n).join(', ') || 'no shared folder'}`
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
        : h('span', { class: 'hint' }, 'none defined yet — ', h('a', { href: '#/services', onclick: (ev) => { ev.preventDefault(); show('services') } }, 'define one')),
      h('span', { class: 'hint' }, s.replaces ? 'Each ticked folder is at /shared/NAME in the isolation.'
        : s.name === 'skill' ? 'Each ticked skill is in Claude Code’s and Codex’s skills there; an untick removes it.'
        : s.forwards ? "Each ticked port is on the Mac's 127.0.0.1, never the LAN. The container stays."
          : 'None ticked: the service with no credential (git: public repositories only).')),
    !noEnvs && !s.replaces && !s.forwards && s.name !== 'skill' && names.length > 1 ? h('div', { class: 'field' }, h('label', null, 'Default'), def, h('span', { class: 'hint' }, 'what the plain variables point at')) : null,
    ctl.rows, err,
    h('div', { class: 'actions' }, apply,
      x.enabled ? h('button', {
        class: 'btn danger', onclick: async () => {
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
        },
      }, 'Turn off') : null,
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
    h('h2', null, 'Sign in to gero'),
    h('p', { class: 'note' }, 'Run ', h('code', null, 'gero web'), ' on the Mac: it opens a signed-in link, and prints one for your phone.'),
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
  if (S.view === 'setup' && !editing('setup')) renderSetup()
}

async function refreshCLIs(names) {
  for (const n of names) S.clis[n] = null
  if (S.view === 'setup' && !editing('setup')) renderSetup()
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
  if (S.view === 'setup' && !editing('setup')) renderSetup()
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
    if (l.state !== was && S.view === 'setup') renderSetup()
  }
}

function cliLoginEnded(iso, tool, l) {
  if (!onScreen(iso, tool)) return
  S.cliLogin = null
  if (l.state === 'done') toast(`${cliName(tool)} is logged in in ${iso}; its idle agents restart with it`)
  else toast(`${cliName(tool)} login: ${l.message || l.state}`, true)
  if (S.view === 'setup') renderSetup()
  guarded(() => refreshCLIs([iso]))
}

async function startCLILogin(iso, tool) {
  if (S.cliLogin) await cancelCLILogin()
  S.cliBusy = `${iso}/${tool}`
  renderSetup()
  await guarded(async () => {
    const l = await api('POST', cliPath(iso, tool) + '/login')
    if (liveLogin(l)) showCLILogin(iso, tool, l)
    else toast(`${cliName(tool)} login: ${l.message || l.state}`, true)
  })
  S.cliBusy = null
  renderSetup()
}

async function cancelCLILogin() {
  const cur = S.cliLogin
  if (!cur) return
  S.cliLogin = null
  renderSetup()
  await guarded(() => api('DELETE', cliPath(cur.iso, cur.tool) + '/login'))
}

async function cliLogout(iso, tool) {
  if (!await ask({ title: `Log ${cliName(tool)} out in ${iso}?`, body: 'Its agents there stop working until it logs in again.', ok: 'Log out', danger: true })) return
  S.cliBusy = `${iso}/${tool}`
  renderSetup()
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

function renderSetup() {
  const page = document.getElementById('setup')
  if (!page) return
  page.replaceChildren(h('div', { class: 'page-inner' },
    h('h2', null, 'Accounts'),
    h('p', { class: 'lead' }, 'An agent CLI uses an account from the Mac, which any number of isolations can share, or a login inside its isolation. That login runs inside the isolation, so any browser can finish it, a phone’s included.'),
    accountsCard(),
    S.isolations.length ? S.isolations.map(setupCard)
      : h('div', { class: 'empty' }, h('p', null, 'No isolations yet.'),
        h('a', { href: '#/isolations', onclick: (e) => { e.preventDefault(); show('isolations') } }, 'Create one'))))
}

function setupCard(i) {
  const cs = S.clis[i.name]
  let body
  if (cs == null) body = h('p', { class: 'note' }, 'Reading the logins from inside the isolation…')
  else if (!Array.isArray(cs)) body = h('p', { class: 'form-error' }, cs.error)
  else body = cs.map((c) => cliRow(i, c))
  return h('div', { class: 'card' },
    h('div', { class: 'head' }, h('h3', null, i.name), h('span', { class: 'chip ' + (i.state === 'running' ? '' : 'blocked') }, i.state)),
    body)
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
    renderSetup()
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

function accountsCard() {
  if (S.accounts == null) return h('div', { class: 'card' }, h('p', { class: 'note' }, 'Reading the accounts…'))
  const usedBy = (a) => a.used_by?.length ? 'used by ' + a.used_by.join(', ') : 'unused'
  const rows = S.accounts.map((a) => h('div', { class: 'cli' },
    h('div', { class: 'head' }, h('span', { class: 'cli-name' }, a.name), h('span', { class: 'ver' }, a.kind),
      a.secret_error ? h('span', { class: 'chip error' }, 'no secret') : null),
    h('div', { class: 'facts' }, usedBy(a)),
    S.openForm === 'acct-secret:' + a.name ? accountSecretForm(a)
      : h('div', { class: 'actions' },
        h('button', { class: 'btn', onclick: () => { S.openForm = 'acct-secret:' + a.name; renderSetup() } }, 'Replace secret'),
        h('button', {
          class: 'btn danger', onclick: async () => {
            const used = a.used_by?.length ? ` ${a.used_by.join(', ')} go back to a login inside.` : ''
            if (!await ask({ title: `Remove account ${a.name}?`, body: `Its secret is deleted.${used}`, ok: 'Remove', danger: true })) return
            await guarded(async () => { await api('DELETE', `/v1/accounts/${enc(a.name)}?force=1`); toast(`Account ${a.name} removed`) })
            await guarded(refreshAccounts)
            if (a.used_by?.length) await guarded(() => refreshCLIs(a.used_by))
          },
        }, 'Remove'))))
  return h('div', { class: 'card' },
    h('div', { class: 'head' }, h('h3', null, 'Shared accounts'),
      S.openForm === 'acct-new' ? null : h('button', { class: 'btn', onclick: () => { S.openForm = 'acct-new'; renderSetup() } }, 'New account')),
    h('p', { class: 'note' }, 'Defined once, on the Mac; any number of isolations can use one. A ChatGPT login cannot be shared this way (its refresh token is single-use), so Codex logs in inside each isolation.'),
    S.openForm === 'acct-new' ? newAccountForm() : null,
    rows.length ? rows : null)
}

function newAccountForm() {
  const name = h('input', { autocomplete: 'off', placeholder: 'e.g. claude' })
  const kind = dropdown(S.accountKinds.map((k) => k.name), { label: 'Kind' })
  const hint = h('span', { class: 'hint' })
  const showHint = () => { hint.textContent = S.accountKinds.find((k) => k.name === kind.value)?.summary || '' }
  kind.addEventListener('change', showHint)
  showHint()
  const secret = h('input', { type: 'password', autocomplete: 'new-password', placeholder: 'sk-ant-…' })
  const err = h('p', { class: 'form-error' })
  const save = h('button', { class: 'btn primary' }, 'Add account')
  save.addEventListener('click', async () => {
    err.textContent = ''
    try {
      await api('POST', `/v1/accounts/${enc(name.value.trim())}`, { kind: kind.value, secret: secret.value.trim() })
      S.openForm = null
      toast(`Account ${name.value.trim()} added; choose it for a CLI below`)
      await refreshAccounts()
      renderSetup()
    } catch (x) {
      if (x instanceof Unauthorized) { S.authed = false; render(); return }
      err.textContent = x.message
    }
  })
  return h('div', { class: 'subform' },
    h('div', { class: 'row' }, h('div', { class: 'field' }, h('label', null, 'Name'), name), h('div', { class: 'field' }, h('label', null, 'Kind'), kind, hint)),
    h('div', { class: 'field' }, h('label', null, 'Secret'), secret,
      h('span', { class: 'hint' }, 'A token from ', h('code', null, 'claude setup-token'), ' (run it on any machine with a browser), or an API key. Stored in the Mac’s credentials file (0600), never shown again.')),
    err,
    h('div', { class: 'actions' }, save, h('button', { class: 'btn', onclick: () => { S.openForm = null; renderSetup() } }, 'Cancel')))
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
      renderSetup()
    } catch (x) {
      if (x instanceof Unauthorized) { S.authed = false; render(); return }
      err.textContent = x.message
    }
  })
  return h('div', { class: 'subform' }, h('div', { class: 'field' }, secret), err,
    h('div', { class: 'actions' }, save, h('button', { class: 'btn', onclick: () => { S.openForm = null; renderSetup() } }, 'Cancel')))
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
      if (liveLogin(r)) { if (onScreen(iso, tool)) { S.cliLogin.login = r; renderSetup() } }
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
  ['Esc', 'Leave the message box'],
  ['j / k', 'Next / previous agent'],
  ['n', 'New agent'],
  ['m', 'Mail of the agent'],
  ['c', 'Cron of the agent'],
  ['t', 'TODO of the agent'],
  ['e', 'Name and ID of the agent'],
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

// stepAgent opens the agent by from the open one in the sidebar's live list.
function stepAgent(by) {
  const rows = [...document.querySelectorAll('#sidebar .sortable:first-of-type > .agent')]
  if (!rows.length) return
  const ids = rows.map((r) => S.agents.find((a) => !a.archived_at && (a.line || a.id) === r.dataset.line)?.id)
  const at = ids.indexOf(S.view === 'agent' ? S.sel : null)
  const next = ids[at < 0 ? (by > 0 ? 0 : ids.length - 1) : Math.min(Math.max(at + by, 0), ids.length - 1)]
  if (next) { select(next); rows[ids.indexOf(next)].scrollIntoView({ block: 'nearest' }) }
}

let goPending = 0
document.addEventListener('keydown', (e) => {
  const t = e.target
  // Esc leaves the message box, once nothing in it (a completion) took it.
  if (e.key === 'Escape' && t.matches?.('.composer textarea') && !e.defaultPrevented) { t.blur(); return }
  if (e.ctrlKey || e.metaKey || e.altKey || e.isComposing || e.defaultPrevented) return
  if (t.closest?.('input, textarea, select, [contenteditable]') || document.querySelector('dialog[open]')) return
  const key = e.code?.startsWith('Key') ? e.code.slice(3).toLowerCase() : e.key
  const a = S.view === 'agent' ? agentById(S.sel) : null
  const go = Date.now() - goPending < 1500
  goPending = 0
  const act = go ? { i: () => show('isolations'), s: () => show('services'), a: () => show('setup') }[key]
    : e.key === '?' ? showShortcuts
    : e.shiftKey ? null
    : {
      '/': () => document.querySelector('.composer textarea:not(:disabled)')?.focus(),
      j: () => stepAgent(1),
      k: () => stepAgent(-1),
      n: () => show('new'),
      m: a && (() => openMail(a)),
      c: a && !a.archived_at && (() => openCrons(a)),
      t: a && !a.archived_at && (() => openTodos(a)),
      e: a && (() => editAgent(a)),
      g: () => { goPending = Date.now() },
    }[key]
  if (!act) return
  e.preventDefault()
  act()
})

route()
startPolling()
