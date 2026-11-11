// PDF 文档翻译 — 内网网页版，对应小程序流程：选文件 → 语言/引擎 → 进度 → 查看/下载 → 历史。
// 与网关的 /api/login、/api/engines、/api/translate* 通信；无构建步骤。
(() => {
  'use strict'

  const MAX_BYTES = 30 * 1024 * 1024
  const LANGUAGES = [
    { label: '自动检测', value: 'auto' },
    { label: '中文（简体）', value: 'zh' },
    { label: '英语', value: 'en' },
    { label: '日语', value: 'ja' },
    { label: '韩语', value: 'ko' },
    { label: '法语', value: 'fr' },
    { label: '德语', value: 'de' },
    { label: '西班牙语', value: 'es' },
  ]
  const LANG_LABEL = Object.fromEntries(LANGUAGES.map(l => [l.value, l.label]))
  const ENGINE_HINT = { Bing: '免费标准翻译，速度快' }
  const AI_HINT = 'AI 大模型翻译，译文更自然，适合论文与合同'

  const $ = id => document.getElementById(id)
  const store = {
    get: (k, d) => { try { return JSON.parse(localStorage.getItem('pdf.' + k)) ?? d } catch (_) { return d } },
    set: (k, v) => localStorage.setItem('pdf.' + k, JSON.stringify(v)),
    del: k => localStorage.removeItem('pdf.' + k),
  }

  let file = null
  let current = null // { id, name, ... } task being tracked
  let pollTimer = null
  let uploadXhr = null

  // ---------- helpers ----------
  function toast(message) {
    const el = $('toast')
    el.textContent = message
    el.classList.remove('hidden')
    clearTimeout(toast.t)
    toast.t = setTimeout(() => el.classList.add('hidden'), 3000)
  }
  function show(id, on = true) { $(id).classList.toggle('hidden', !on) }
  function formatSize(bytes) {
    return bytes >= 1048576 ? (bytes / 1048576).toFixed(1) + ' MB' : Math.max(1, Math.round(bytes / 1024)) + ' KB'
  }
  function randomId() {
    const bytes = new Uint8Array(16)
    crypto.getRandomValues(bytes) // available on plain-http LAN origins, unlike crypto.randomUUID
    return Array.from(bytes, b => b.toString(16).padStart(2, '0')).join('')
  }
  function deviceId() {
    let id = store.get('device')
    if (!id) { id = randomId(); store.set('device', id) }
    return id
  }

  // ---------- auth + api ----------
  class NeedPassword extends Error {}

  async function login(username, password) {
    const res = await fetch('/api/login', {
      method: 'POST', headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ device_id: deviceId(), username, password }),
    })
    const body = await res.json().catch(() => ({}))
    if (!res.ok) {
      if (res.status === 401) throw new NeedPassword(body.error || '访问密码错误')
      throw new Error(body.error || '登录失败：' + res.status)
    }
    store.set('token', body.token)
    return body.token
  }

  async function ensureToken() {
    const token = store.get('token')
    if (token) return token
    throw new NeedPassword('请登录')
  }

  async function authFetch(path, options = {}, retry = true) {
    const token = await ensureToken()
    const res = await fetch(path, { ...options, headers: { ...(options.headers || {}), Authorization: 'Bearer ' + token } })
    if (res.status === 401 && retry) {
      store.del('token')
      return authFetch(path, options, false)
    }
    return res
  }

  async function errorOf(res, fallback) {
    const body = await res.json().catch(() => ({}))
    return body.error || fallback + '：' + res.status
  }

  let modelConfigs = []
  function editModel(id) {
    const config = modelConfigs.find(c => c.id === id) || {}
    $('model-select').value = config.id || ''
    $('model-label').value = config.label || ''
    $('model-url').value = config.base_url || ''
    $('model-name').value = config.model || ''
    $('model-key').value = ''
    $('model-key').placeholder = config.has_api_key ? '已设置，留空保持不变' : '输入 API Key'
    $('model-key-hint').textContent = config.has_api_key ? '密钥已保存，留空不会修改。' : '密钥仅保存在服务器。'
    $('model-error').textContent = ''
  }
  async function loadModelConfig(selected) {
    const res = await authFetch('/api/model-config')
    if (!res.ok) throw new Error(await errorOf(res, '读取模型配置失败'))
    const data = await res.json()
    modelConfigs = data.configs || []
    fillSelect($('model-select'), [{ value: '', label: '新配置' }, ...modelConfigs.map(c => ({ value: c.id, label: c.label }))], selected || modelConfigs[0]?.id || '')
    editModel($('model-select').value)
    if (!modelConfigs.length) $('model-details').open = true
  }

  async function saveModelConfig(e) {
    e.preventDefault()
    $('model-error').textContent = ''
    $('model-save').disabled = true
    try {
      const res = await authFetch('/api/model-config', {
        method: 'PUT', headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ id: $('model-select').value, label: $('model-label').value.trim(), base_url: $('model-url').value.trim(), model: $('model-name').value.trim(), api_key: $('model-key').value }),
      })
      if (!res.ok) throw new Error(await errorOf(res, '保存模型配置失败'))
      const saved = await res.json()
      await loadModelConfig(saved.id)
      await loadEngines(saved.id)
      $('model-details').open = false
      toast('模型配置已保存')
    } catch (err) { $('model-error').textContent = err.message }
    finally { $('model-save').disabled = false }
  }

  // ---------- form ----------
  function fillSelect(select, items, selected) {
    select.innerHTML = ''
    for (const item of items) {
      const option = document.createElement('option')
      option.value = item.value
      option.textContent = item.label
      select.appendChild(option)
    }
    select.value = selected
  }

  function initLanguages() {
    const saved = store.get('langs', { in: 'en', out: 'zh' })
    fillSelect($('lang-in'), LANGUAGES, saved.in)
    fillSelect($('lang-out'), LANGUAGES.slice(1), saved.out)
  }
  function saveLangs() { store.set('langs', { in: $('lang-in').value, out: $('lang-out').value }) }

  async function loadEngines(preferred) {
    const res = await authFetch('/api/engines')
    if (!res.ok) throw new Error(await errorOf(res, '读取翻译引擎失败'))
    const { engines } = await res.json()
    const select = $('engine')
    select.innerHTML = ''
    for (const name of engines) {
      const option = document.createElement('option')
      option.value = name
      option.textContent = modelConfigs.find(c => c.id === name)?.label || name
      select.appendChild(option)
    }
    const saved = preferred || store.get('engine')
    if (engines.includes(saved)) select.value = saved
    updateEngineHint()
    updateSubmit()
  }
  function updateEngineHint() {
    const name = $('engine').value
    $('engine-hint').textContent = name ? (ENGINE_HINT[name] || AI_HINT) : '请先保存上方 AI 模型配置'
    store.set('engine', name)
  }

  function setFile(next) {
    $('form-error').textContent = ''
    if (next) {
      if (!/\.pdf$/i.test(next.name)) return void ($('form-error').textContent = '请选择 PDF 文件')
      if (next.size > MAX_BYTES) return void ($('form-error').textContent = '文件超过 30 MB 限制（' + formatSize(next.size) + '）')
      if (next.size === 0) return void ($('form-error').textContent = '文件为空')
    }
    file = next
    $('dropzone').classList.toggle('has-file', !!file)
    show('drop-empty', !file)
    show('drop-file', !!file)
    if (file) { $('file-name').textContent = file.name; $('file-size').textContent = formatSize(file.size) }
    if (!file) $('file-input').value = ''
    updateSubmit()
  }
  function updateSubmit() { $('submit').disabled = !file || !$('engine').value }

  // ---------- translate ----------
  function showView(view) {
    show('form-card', view === 'form')
    show('progress-card', view === 'progress')
    show('result-card', view === 'result')
  }

  function setProgress(percent, title, detail) {
    const value = Math.max(0, Math.min(100, Math.round(percent || 0)))
    $('progress-percent').textContent = value + '%'
    $('progress-fill').style.width = value + '%'
    if (title) $('progress-title').textContent = title
    $('progress-detail').textContent = detail || ''
  }

  function submit() {
    if (!file) return
    const task = {
      name: file.name, langIn: $('lang-in').value, langOut: $('lang-out').value,
      engine: $('engine').value, engineLabel: $('engine').selectedOptions[0]?.textContent || '', createdAt: Date.now(), state: 'uploading',
    }
    const form = new FormData()
    form.append('file', file, file.name)
    form.append('data', JSON.stringify({ lang_in: task.langIn, lang_out: task.langOut, engine: task.engine }))

    showView('progress')
    show('cancel', true); show('back-from-progress', false)
    $('progress-file').textContent = task.name + ' · ' + (task.engineLabel || task.engine)
    $('progress-error').textContent = ''
    setProgress(0, '正在上传', '上传中 0%')

    ensureToken().then(token => {
      const xhr = new XMLHttpRequest()
      uploadXhr = xhr
      xhr.open('POST', '/api/translate')
      xhr.setRequestHeader('Authorization', 'Bearer ' + token)
      xhr.upload.onprogress = e => {
        if (e.lengthComputable) setProgress(0, '正在上传', '上传中 ' + Math.round(e.loaded * 100 / e.total) + '%')
      }
      xhr.onerror = () => failUpload('网络错误，上传失败')
      xhr.onabort = () => {}
      xhr.onload = () => {
        uploadXhr = null
        let body = {}
        try { body = JSON.parse(xhr.responseText) } catch (_) {}
        if (xhr.status === 401) { store.del('token'); return void submit() }
        if (xhr.status !== 200 || !body.id) return failUpload(body.error || '上传失败：' + xhr.status)
        task.id = body.id
        task.state = 'running'
        current = task
        saveHistory(task)
        setProgress(0, '正在翻译', '已提交，等待开始…')
        poll()
      }
      xhr.send(form)
    }).catch(e => failUpload(e instanceof NeedPassword ? '登录已失效，请刷新页面' : e.message))
  }

  function failUpload(message) {
    uploadXhr = null
    $('progress-error').textContent = message
    show('cancel', false); show('back-from-progress', true)
  }

  function poll() {
    clearTimeout(pollTimer)
    if (!current) return
    const task = current
    const tick = async () => {
      if (current !== task) return
      try {
        const res = await authFetch('/api/translate/' + encodeURIComponent(task.id))
        if (res.status === 404) throw Object.assign(new Error('任务已不存在（服务可能已重启）'), { gone: true })
        if (!res.ok) throw new Error(await errorOf(res, '查询进度失败'))
        const st = await res.json()
        if (current !== task) return
        $('progress-error').textContent = ''
        if (st.state === 'queued') {
          setProgress(0, '排队中', st.queue_position > 0 ? '排队第 ' + st.queue_position + ' 位，轮到后自动开始' : '等待开始…')
        } else if (st.state === 'finished') {
          return void finish(task, st)
        } else if (st.state === 'error' || st.state === 'cancelled') {
          task.state = st.state; saveHistory(task)
          $('progress-error').textContent = st.state === 'cancelled' ? '翻译已取消' : (st.error || '翻译失败')
          show('cancel', false); show('back-from-progress', true)
          setProgress(Number(st.progress) || 0, st.state === 'cancelled' ? '已取消' : '翻译失败', '')
          return void renderHistory()
        } else {
          setProgress(st.progress, '正在翻译', st.stage || '正在处理…')
        }
      } catch (e) {
        if (e.gone) {
          task.state = 'expired'; saveHistory(task); renderHistory()
          $('progress-error').textContent = e.message
          show('cancel', false); show('back-from-progress', true)
          return
        }
        $('progress-detail').textContent = '连接异常，正在重试…（' + e.message + '）'
      }
      pollTimer = setTimeout(tick, 2000)
    }
    tick()
  }

  function finish(task, st) {
    task.state = 'finished'
    task.hasMono = !!st.mono_url
    saveHistory(task)
    renderHistory()
    $('result-file').textContent = task.name + ' · ' + (LANG_LABEL[task.langIn] || task.langIn) + ' → ' + (LANG_LABEL[task.langOut] || task.langOut) + ' · ' + (task.engineLabel || task.engine)
    document.querySelectorAll('[data-needs=mono]').forEach(el => el.classList.toggle('hidden', !task.hasMono))
    showView('result')
  }

  async function cancelTask() {
    if (uploadXhr) { uploadXhr.abort(); uploadXhr = null; return void backToForm() }
    if (!current) return backToForm()
    const task = current
    clearTimeout(pollTimer)
    $('cancel').disabled = true
    try { await authFetch('/api/translate/' + encodeURIComponent(task.id), { method: 'DELETE' }) } catch (_) {}
    $('cancel').disabled = false
    task.state = 'cancelled'; saveHistory(task); renderHistory()
    backToForm()
  }

  function backToForm() {
    clearTimeout(pollTimer)
    current = null
    showView('form')
  }

  // ---------- result files ----------
  async function fetchPdf(task, kind) {
    const res = await authFetch('/api/translate/' + encodeURIComponent(task.id) + '/' + kind)
    if (!res.ok) throw Object.assign(new Error(res.status === 404 ? '译文已不存在（服务可能已重启或已清理）' : await errorOf(res, '获取译文失败')), { status: res.status })
    return URL.createObjectURL(new Blob([await res.arrayBuffer()], { type: 'application/pdf' }))
  }

  async function openPdf(task, kind) {
    // Open the tab synchronously so popup blockers allow it; navigate once the PDF is fetched.
    const tab = window.open('', '_blank')
    try {
      const url = await fetchPdf(task, kind)
      if (tab) tab.location.href = url
      else downloadUrl(url, task, kind)
    } catch (e) {
      if (tab) tab.close()
      toast(e.message)
      if (e.status === 404) { task.state = 'expired'; saveHistory(task); renderHistory() }
    }
  }

  async function downloadPdf(task, kind) {
    try { downloadUrl(await fetchPdf(task, kind), task, kind) } catch (e) {
      toast(e.message)
      if (e.status === 404) { task.state = 'expired'; saveHistory(task); renderHistory() }
    }
  }

  function downloadUrl(url, task, kind) {
    const a = document.createElement('a')
    a.href = url
    a.download = task.name.replace(/\.pdf$/i, '') + (kind === 'dual' ? '.dual.pdf' : '.zh.pdf')
    document.body.appendChild(a); a.click(); a.remove()
    setTimeout(() => URL.revokeObjectURL(url), 60000)
  }

  // ---------- history ----------
  function saveHistory(task) {
    if (!task.id) return
    const list = store.get('history', []).filter(item => item.id !== task.id)
    store.set('history', [{ ...task }, ...list].slice(0, 20))
  }

  const STATE_LABEL = {
    finished: ['已完成', 'ok'], error: ['失败', 'fail'], cancelled: ['已取消', ''],
    expired: ['已过期', ''], uploading: ['处理中', ''], running: ['翻译中', ''], queued: ['排队中', ''],
  }

  function renderHistory() {
    const list = store.get('history', [])
    show('history-card', list.length > 0)
    const ul = $('history-list')
    ul.innerHTML = ''
    for (const task of list) {
      const li = document.createElement('li')
      const label = STATE_LABEL[task.state] || ['翻译中', '']
      const meta = document.createElement('div')
      meta.className = 'meta'
      const name = document.createElement('strong'); name.textContent = task.name; name.title = task.name
      const sub = document.createElement('span')
      sub.textContent = new Date(task.createdAt).toLocaleString('zh-CN', { hour12: false }) + ' · ' +
        (LANG_LABEL[task.langIn] || task.langIn) + ' → ' + (LANG_LABEL[task.langOut] || task.langOut) + ' · ' + (task.engineLabel || task.engine)
      meta.append(name, sub)
      const status = document.createElement('span')
      status.className = 'status ' + label[1]; status.textContent = label[0]
      li.append(meta, status)
      if (task.state === 'finished') {
        li.append(button('查看', () => openPdf(task, 'dual')), button('下载', () => downloadPdf(task, 'dual')))
      } else if (!['error', 'cancelled', 'expired'].includes(task.state) && task.id) {
        li.append(button('进度', () => resume(task)))
      }
      ul.appendChild(li)
    }
  }
  function button(text, onClick) {
    const b = document.createElement('button')
    b.type = 'button'; b.className = 'btn'; b.textContent = text; b.onclick = onClick
    return b
  }

  function resume(task) {
    current = task
    showView('progress')
    show('cancel', true); show('back-from-progress', false)
    $('progress-file').textContent = task.name + ' · ' + (task.engineLabel || task.engine)
    $('progress-error').textContent = ''
    setProgress(0, '正在翻译', '正在获取进度…')
    poll()
  }

  // ---------- boot ----------
  function bind() {
    $('file-input').addEventListener('change', e => setFile(e.target.files[0] || null))
    $('file-remove').addEventListener('click', e => { e.preventDefault(); e.stopPropagation(); setFile(null) })
    const zone = $('dropzone')
    for (const type of ['dragenter', 'dragover']) zone.addEventListener(type, e => { e.preventDefault(); zone.classList.add('over') })
    for (const type of ['dragleave', 'drop']) zone.addEventListener(type, e => { e.preventDefault(); zone.classList.remove('over') })
    zone.addEventListener('drop', e => { const dropped = e.dataTransfer.files[0]; if (dropped) setFile(dropped) })
    $('lang-in').addEventListener('change', saveLangs)
    $('lang-out').addEventListener('change', saveLangs)
    $('engine').addEventListener('change', () => { updateEngineHint(); updateSubmit() })
    $('swap').addEventListener('click', () => {
      const a = $('lang-in').value, b = $('lang-out').value
      if (a === 'auto') return toast('自动检测不能作为目标语言')
      $('lang-in').value = b; $('lang-out').value = a; saveLangs()
    })
    $('submit').addEventListener('click', submit)
    $('cancel').addEventListener('click', cancelTask)
    $('back-from-progress').addEventListener('click', backToForm)
    $('new-task').addEventListener('click', () => { setFile(null); backToForm() })
    document.querySelectorAll('[data-open]').forEach(el => el.addEventListener('click', () => current && openPdf(current, el.dataset.open)))
    document.querySelectorAll('[data-download]').forEach(el => el.addEventListener('click', () => current && downloadPdf(current, el.dataset.download)))
    $('history-clear').addEventListener('click', () => { if (confirm('清空本浏览器中的翻译记录？')) { store.set('history', []); renderHistory() } })
    $('model-form').addEventListener('submit', saveModelConfig)
    $('model-select').addEventListener('change', e => editModel(e.target.value))
    $('model-new').addEventListener('click', () => editModel(''))
    $('logout').addEventListener('click', () => { store.del('token'); clearTimeout(pollTimer); current = null; show('app', false); show('login', true); $('login-password').value = '' })
    $('login-form').addEventListener('submit', async e => {
      e.preventDefault()
      try {
        await login($('login-username').value, $('login-password').value)
        $('login-password').value = ''
        $('login-error').textContent = ''
        start()
      } catch (err) { $('login-error').textContent = err.message }
    })
  }

  async function start() {
    try {
      await ensureToken()
      await loadModelConfig()
      await loadEngines()
      show('login', false); show('app', true)
      renderHistory()
      // Reattach to a translation that was running when the page was closed.
      const active = store.get('history', []).find(t => t.id && ['uploading', 'running', 'queued'].includes(t.state))
      if (active && !current && !file) resume(active)
    } catch (e) {
      if (e instanceof NeedPassword) { show('app', false); show('login', true); return }
      show('login', false); show('app', true)
      $('form-error').textContent = e.message
    }
  }

  initLanguages()
  bind()
  start()
})()
