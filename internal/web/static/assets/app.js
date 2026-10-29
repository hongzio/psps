// gero web UI. Plain DOM, no framework, no build step.
//
// Everything an agent says is untrusted (a repository or a web page can put
// words in its mouth), so agent text only ever reaches the page through
// text nodes: h() below never parses HTML.

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
  // sending }. openRewound holds the earlier versions shown unfolded.
  edit: null,
  openRewound: new Set(),
}

const coarse = matchMedia('(pointer: coarse)').matches
const agentById = (id) => S.agents.find((a) => a.id === id)
// agentTitle is what an agent is called on the page: one made without a
// prompt has no name until its first message.
const agentTitle = (a) => a.name || 'New agent'
// shownID is the ID an agent goes by: its own, or the key gero gave it.
const shownID = (a) => a.handle || a.id
// letter reads a mail from another agent as the agent got it (the daemon's
// mailproxy.Letter): who wrote it, from where, and what it says.
const LETTER = /^\[gero mail #(\d+) from agent (\S+) in isolation (\S+)\. [^\n]*\]\n\n/
function letter(text) {
  const m = LETTER.exec(text)
  return m && { id: m[1], from: m[2], iso: m[3], body: text.slice(m[0].length) }
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
// fields hold text inputs for the answer instead: the first starts
// focused, and Enter in any takes the action. onOk, if given, does the
// action: what it throws is shown in the dialog, which stays open. Without
// ok it is only something to look at, closed with cancel ("Close").
function ask({ title, body, ok, cancel: cancelLabel = 'Cancel', danger, extra, fields, onOk }) {
  return new Promise((resolve) => {
    const cancel = h('button', { class: 'btn', value: 'cancel' }, cancelLabel)
    const go = ok ? h('button', { class: 'btn primary' + (danger ? ' danger' : ''), value: 'ok' }, ok) : null
    const err = h('p', { class: 'form-error dialog-error', role: 'alert', hidden: true })
    const d = h('dialog', { class: 'dialog', 'aria-labelledby': 'dialog-title', 'aria-describedby': body ? 'dialog-body' : null },
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
  const add = (en) => { en.src = e.src; c.entries.push(en); if (en.item) c.byItem.set(en.item, en); return en }
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
      for (const en of c.entries) if (en.kind === 'approval' && en.id === e.approval.id) en.decision = e.approval.decision
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
const askText = (kind) => ({ command: 'run a command', file_change: 'change files', tool: 'use a tool' })[kind] || kind

// richText renders markdown-ish agent text: fenced code, `inline code`,
// **bold**, paragraphs. Text nodes only.
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

async function refreshAgents() {
  const agents = await api('GET', '/v1/agents')
  S.agents = agents
  if (!S.sel && S.view === 'agent') {
    const live = agents.filter((a) => !a.archived_at)
    if (live.length) select(live[live.length - 1].id, false)
  }
  // A deep link (#/a/<id>) is drawn before the list arrives; draw the agent
  // once it is known.
  if (S.view === 'agent' && S.sel && S.drawn !== S.sel && agentById(S.sel)) { render(); return }
  renderSidebar()
  if (S.view === 'agent') renderTopbar()
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
  await guarded(() => api('POST', `/v1/agents/${id}/messages`, { text }))
}

async function decide(approvalID, decision) {
  await guarded(() => api('POST', `/v1/agents/${S.sel}/approvals/${encodeURIComponent(approvalID)}`, { decision }))
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
  root.append(h('aside', { class: 'sidebar', id: 'sidebar' }), h('div', { class: 'scrim', onclick: () => { S.drawer = false; render() } }))
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
    h('button', { class: 'menu-btn', 'aria-label': 'Agents', onclick: () => { S.drawer = true; render() } }, '☰'),
    h('div', { class: 'title' }, h('h1', null, title), sub ? h('div', { class: 'sub' }, sub) : null),
    ...extra)
}

function renderSidebar() {
  const el = document.getElementById('sidebar')
  if (!el) return
  const live = S.agents.filter((a) => !a.archived_at)
  const archived = S.agents.filter((a) => a.archived_at)
  const row = (a) => {
    const st = a.archived_at ? 'idle' : a.state?.status || 'idle'
    const meta = a.problem ? h('span', { class: 'problem' }, '⚠ ' + a.problem)
      : `${a.kind} · ${a.isolation} · ${st}` + (a.inbox ? ` · ✉ ${a.inbox}` : '')
    return h('button', {
      class: 'agent' + (a.id === S.sel && S.view === 'agent' ? ' selected' : '') + (a.archived_at ? ' archived' : ''),
      onclick: () => select(a.id),
    }, h('span', { class: 'dot ' + st }), h('span', { class: 'name' }, agentTitle(a)), h('span', { class: 'meta' }, meta))
  }
  el.replaceChildren(
    h('div', { class: 'brand' }, 'gero', h('span', { class: 'count' }, `${live.length} agent${live.length === 1 ? '' : 's'}`)),
    h('button', { class: 'new-agent' + (S.view === 'new' ? ' active' : ''), onclick: () => show('new') }, '+ New agent'),
    h('div', { class: 'agent-list' },
      live.map(row),
      h('button', { class: 'fold', onclick: () => { S.showArchived = !S.showArchived; renderSidebar() } },
        `${S.showArchived ? '▾' : '▸'} Archived (${archived.length})`),
      S.showArchived ? archived.map(row) : null),
    h('div', { class: 'side-foot' },
      h('button', { class: S.view === 'isolations' ? 'active' : '', onclick: () => show('isolations') }, '⚙ Isolations'),
      h('button', { class: S.view === 'services' ? 'active' : '', onclick: () => show('services') }, '⚿ Services'),
      h('button', { class: S.view === 'setup' ? 'active' : '', onclick: () => show('setup') }, '✦ Accounts')),
  )
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

function renderTopbar() {
  const slot = document.getElementById('topbar-slot')
  const a = agentById(S.sel)
  if (!slot || !a) return
  const st = a.archived_at ? 'archived' : a.state?.status || 'idle'
  const working = st === 'working'
  // replaceChildren turns null into the text "null"; keep only nodes.
  slot.replaceChildren(...[
    topbar(agentTitle(a), `${a.kind} · ${a.isolation} · ${shownID(a)}`,
      h('span', { class: 'chip ' + st }, st),
      h('div', { class: 'actions' },
        working ? h('button', { class: 'btn', onclick: () => guarded(() => api('POST', `/v1/agents/${a.id}/interrupt`)) }, '■', h('span', { class: 'label' }, ' Stop')) : null,
        h('button', { class: 'btn ghost', title: 'Its mail: what it was sent and what it wrote', onclick: () => openMail(a) },
          '✉', h('span', { class: 'label' }, ' Mail'), a.inbox ? h('span', { class: 'badge' }, String(a.inbox)) : null),
        h('button', { class: 'btn ghost', title: 'Name and ID', onclick: () => editAgent(a) }, '✎', h('span', { class: 'label' }, ' Name & ID')),
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
        }, '🗑', h('span', { class: 'label' }, ' Delete')) : null,
        a.archived_at ? null : h('button', {
          class: 'btn ghost', title: 'Archive', onclick: async () => {
            if (!await ask({ title: `Archive "${agentTitle(a)}"?`, body: 'Its history is kept.', ok: 'Archive' })) return
            await guarded(() => api('POST', `/v1/agents/${a.id}/archive`))
            S.sel = null
            await guarded(refreshAgents)
            render()
          },
        }, '🗄', h('span', { class: 'label' }, ' Archive')))),
    a.problem ? h('div', { class: 'banner' }, a.problem) : null,
  ].filter(Boolean))
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
  const items = c.entries.map((en) => entryView(en, a)).filter(Boolean)
  const none = c.ready ? 'Nothing yet: the first message you send starts it.' : 'Loading…'
  inner.replaceChildren(...(items.length ? items : [h('div', { class: 'empty' }, none)]))
  const ta = typing && inner.querySelector('.edit-box')
  if (ta) { ta.focus({ preventScroll: true }); ta.setSelectionRange(...sel) }
  if (atBottom) box.scrollTop = box.scrollHeight
}

// entryView draws one entry; frozen ones are an earlier version, there to
// read only.
function entryView(en, a, frozen) {
  switch (en.kind) {
    case 'user': {
      const l = letter(en.text)
      if (l) return h('div', { class: 'msg-mail' }, h('div', { class: 'who' }, `✉ Mail #${l.id} from ${l.from} · ${l.iso}`), h('div', { class: 'text' }, richText(l.body)))
      if (!frozen && S.edit?.agent === a.id && S.edit.id === en.id) return editView(en, a)
      const bubble = h('div', { class: 'msg-user text' }, richText(en.text))
      if (frozen || !en.id || !rewindable(a)) return bubble
      return h('div', { class: 'msg-user-row' },
        h('button', { class: 'edit-msg', title: 'Edit and resend: the agent goes back to here', 'aria-label': 'Edit message', onclick: () => startEdit(a, en) }, '✎'),
        bubble)
    }
    case 'rewound': {
      const key = `${a.id}:${en.src}`
      const n = en.entries.filter((x) => x.kind === 'user' || x.kind === 'assistant').length
      return h('details', {
        class: 'rewound', open: S.openRewound.has(key),
        ontoggle: (e) => { if (e.target.open) S.openRewound.add(key); else S.openRewound.delete(key) },
      },
        h('summary', null, `↺ Edited · the earlier version (${n} message${n === 1 ? '' : 's'}) is gone from the agent's memory`),
        h('div', { class: 'rewound-body' }, en.entries.map((x) => entryView(x, a, true)).filter(Boolean)))
    }
    case 'assistant':
      return h('div', { class: 'msg-agent' + (en.live ? ' live' : '') }, h('div', { class: 'who' }, label(a)), h('div', { class: 'text' }, richText(en.text)))
    case 'reasoning': return h('div', { class: 'reasoning' }, en.text)
    case 'tool': {
      const mark = { ok: '✓', failed: '✗', running: '…' }[en.state]
      const out = (en.output || '').trim()
      return h('details', { class: 'tool' },
        h('summary', null, h('span', { class: 'mark ' + en.state }, mark), h('span', { class: 'tname' }, en.name), h('span', { class: 'tsum' }, en.summary)),
        out ? h('pre', null, clip(out, 40)) : null)
    }
    case 'approval': {
      const d = en.decision
      const outcome = d ? ' — ' + decisionText(d) : ''
      return h('div', { class: 'approval' + (d ? ' decided' : '') },
        h('div', { class: 'head' }, `Approval: ${en.akind}${outcome}`),
        h('div', { class: 'what' }, en.summary),
        en.reason && !d ? h('div', { class: 'why' }, en.reason) : null)
    }
    case 'interrupted': return h('div', { class: 'interrupted' }, '⏸ This turn was interrupted. Send a message to continue.')
    case 'error': return h('div', { class: 'error-line' }, '✗ ' + en.text)
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
  const mails = c.entries.slice(c.entries.indexOf(en) + 1).filter((x) => x.kind === 'user' && letter(x.text)).length
  const ok = rewindable(a)
  const note = !ok ? 'The agent is busy; resend once it is idle, or stop it.'
    : 'The agent forgets this message and everything after it; files stay as they are.' +
      (mails ? ` That includes ${mails} mail${mails === 1 ? '' : 's'}, not delivered again.` : '')
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

function composer(a) {
  const ta = h('textarea', {
    rows: 1,
    placeholder: a.archived_at ? 'This agent is archived.' : a.name ? 'Message the agent…' : 'What should the agent do?',
    title: coarse ? null : 'Enter sends · Shift+Enter adds a line',
    disabled: !!a.archived_at,
    value: S.drafts.get(a.id) || '',
  })
  const grow = () => { ta.style.height = 'auto'; ta.style.height = Math.min(ta.scrollHeight, 200) + 'px' }
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
  ta.addEventListener('keydown', (e) => {
    if (e.key === 'Enter' && !e.shiftKey && !coarse && !e.isComposing) { e.preventDefault(); submit() }
    if (e.key === 'Escape') { S.settingsOpen = false; renderSettings(a) }
  })
  queueMicrotask(grow)
  const wrap = h('div', { class: 'composer' },
    h('div', { id: 'settings-slot' }),
    h('div', { class: 'composer-row' },
      ta,
      h('button', {
        class: 'icon-btn' + (S.settingsOpen ? ' on' : ''), 'aria-label': 'Agent settings', title: 'Agent settings',
        disabled: !!a.archived_at, onclick: () => { S.settingsOpen = !S.settingsOpen; renderSettings(a) },
      }, '⚙'),
      h('button', { class: 'btn primary send-btn', disabled: !!a.archived_at, onclick: submit }, 'Send')))
  queueMicrotask(() => renderSettings(a))
  return wrap
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
  const create = h('button', { class: 'btn primary' }, 'Create agent')
  create.addEventListener('click', async () => {
    err.textContent = ''
    if (!iso.value) { err.textContent = 'Create an isolation first.'; return }
    create.disabled = true
    create.textContent = 'Creating…'
    try {
      const a = await api('POST', '/v1/agents', {
        isolation: iso.value, kind, handle: handle.value.trim() || undefined,
        options: { effort: effort.value || undefined, model: model.value.trim() || undefined, approvals: approvals.value },
      })
      await refreshAgents()
      select(a.id)
      // It waits for its first message.
      document.querySelector('.composer textarea')?.focus()
    } catch (e) {
      if (e instanceof Unauthorized) { S.authed = false; render(); return }
      err.textContent = e.message
      create.disabled = false
      create.textContent = 'Create agent'
    }
  })
  return h('div', { class: 'page' }, h('div', { class: 'page-inner' },
    h('h2', null, 'New agent'),
    h('p', { class: 'lead' }, 'It runs inside an isolation, in a workspace of its own, and starts on the first message you send it.'),
    noIso,
    h('div', { class: 'row' },
      h('div', { class: 'field' }, h('label', null, 'Isolation'), iso),
      h('div', { class: 'field' }, h('label', null, 'Agent'), seg)),
    h('div', { class: 'field' }, h('label', null, 'ID'), handle,
      h('span', { class: 'hint' }, 'Lowercase letters, digits and dashes; unique among all agents, archived ones too. Left empty, it gets an adjective-noun no agent has.')),
    h('div', { class: 'row' },
      h('div', { class: 'field' }, h('label', null, 'Effort'), effort),
      h('div', { class: 'field' }, h('label', null, 'Model'), model)),
    h('div', { class: 'field' }, h('label', null, 'Approvals'), approvals),
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
        h('span', { class: 'chip ' + (i.fence_state === 'verified' ? '' : i.fence_state === 'off' ? 'blocked' : 'error') }, fenceText(i) || 'fence ✗')),
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
            : [(x.envs || []).join(', ') || 'no env', x.default_env ? `(default ${x.default_env})` : '', compactConfig(x.settings)].filter(Boolean).join(' ')
      return h('div', { class: 'svc-row' },
        h('div', { class: 'svc-line' },
          h('span', { class: 'dot' + (x.enabled ? ' on' : '') }),
          h('strong', null, s.name), h('span', { class: 'facts' }, detail),
          x.missing?.length ? h('span', { class: 'chip error' }, 'missing ' + x.missing.join(',')) : null,
          S.openForm === key ? null : h('button', { class: 'btn small', onclick: () => openForm(key) }, x.enabled ? 'Change' : 'Turn on')),
        S.openForm === key ? exposeForm(i, s, x) : null)
    }))
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
      await api('PUT', `/v1/isolations/${enc(i.name)}/services/${enc(s.name)}`,
        { envs, default_env: def.value === DEFAULT ? undefined : def.value, settings: ctl.patch() })
      S.openForm = null
      toast(s.replaces ? `${i.name} has ${envs.map((n) => '/shared/' + n).join(', ') || 'no shared folder'}`
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
  // A share says which folder of the Mac's each env is.
  const envNote = (n) => {
    if (!s.replaces) return null
    const c = S.envs.find((e) => e.service === s.name && e.name === n)?.config || {}
    return h('span', { class: 'hint' }, `${c.path || '?'} · ${c.read_write ? 'read-write' : 'read-only'}`)
  }
  return h('div', { class: 'subform' },
    noEnvs ? null : h('div', { class: 'field' }, h('label', null, 'Environments'),
      names.length ? h('div', { class: 'ticks' }, boxes.map(({ n, el }) => h('label', { class: 'check' }, el, n, envNote(n))))
        : h('span', { class: 'hint' }, 'none defined yet — ', h('a', { href: '#/services', onclick: (ev) => { ev.preventDefault(); show('services') } }, 'define one')),
      h('span', { class: 'hint' }, s.replaces ? 'Each ticked folder is at /shared/NAME in the isolation.'
        : 'None ticked: the service with no credential (git: public repositories only).')),
    !noEnvs && !s.replaces && names.length > 1 ? h('div', { class: 'field' }, h('label', null, 'Default'), def, h('span', { class: 'hint' }, 'what the plain variables point at')) : null,
    ctl.rows, err,
    h('div', { class: 'actions' }, apply,
      x.enabled ? h('button', {
        class: 'btn danger', onclick: async () => {
          if (!await ask({
            title: `Turn ${s.name} off for ${i.name}?`,
            body: s.replaces ? 'Its container is replaced without the shared folders: running agents end and start again on their next message.'
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

route()
startPolling()
