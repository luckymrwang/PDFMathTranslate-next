// node homepage-miniapp.test.cjs /path/to/pdftranslate
const assert = require('node:assert/strict')
const fs = require('node:fs')
const path = require('node:path')
const vm = require('node:vm')
const root = process.argv[2]
assert.ok(root)
async function run() {
  let page, created = [], navigated = 0, relaunched = 0
  const app = { globalData: {} }, cache = {}
  const realPayment = require(path.join(root, 'utils/payment.js'))
  const payment = { ...realPayment, isIOS: () => false, assertPaymentSupported() {},
    pay: async () => { throw { errMsg: 'requestVirtualPayment:fail cancel' } } }
  const config = { enabled: true, trial_enabled: true, trial_available: true, trial_pages: 3, trial_engine: 'Bing',
    standard_price_fen: 10, enhanced_price_fen: 30, engine_prices_fen: { 'GPT-6': 40 } }
  let orders = [], freeOrders = false
  const api = {
    getPayConfig: async () => config,
    createPreparedOrder: async (token, data) => {
      created.push({ token, data })
      if (data.trial) return { order: { id: 'trial-1', engine: 'Bing', pages: 3 } }
      return freeOrders ? { order: { id: 'free-1', state: 'paid' } } : { order: { id: 'paid-1' }, payData: {} }
    },
    listPayOrders: async () => ({ orders }),
    cancelPayOrder: async () => ({}),
    queryPayOrder: async () => ({ state: 'paid' }),
    warmEngine: async () => ({}),
  }
  const sandbox = {
    require: name => {
      if (name.includes('storage-key')) return { storageKey: k => 'dev:' + k }
      if (name.endsWith('/api')) return api
      if (name.endsWith('/config')) return { demoMode: false }
      if (name.endsWith('/payment')) return payment
      if (name.endsWith('/order-status')) return vm.runInNewContext(
        fs.readFileSync(path.join(root, 'utils/order-status.js'), 'utf8') + ';module.exports',
        { module: { exports: {} }, require: n => n.endsWith('/payment') ? payment : { storageKey: k => 'dev:' + k }, wx: sandbox.wx })
      return {}
    },
    Page: p => { page = p },
    getApp: () => app,
    wx: {
      getStorageSync: k => cache[k], setStorageSync: (k, v) => { cache[k] = v },
      navigateTo: () => navigated++, showToast() {}, reLaunch: () => relaunched++,
    }, console, setTimeout,
  }
  const load = file => {
    vm.runInNewContext(fs.readFileSync(path.join(root, file), 'utf8'), { ...sandbox })
    const loaded = page
    loaded.data = JSON.parse(JSON.stringify(loaded.data))
    loaded.setData = (data, done) => { Object.assign(loaded.data, data); if (done) done() }
    return loaded
  }

  const home = load('pages/index/index.js')
  home.onLoad({ ref: 'INVITE23' })
  home.onLoad({ ref: 'OTHER234' })
  assert.equal(cache['dev:invite_ref'], 'INVITE23', 'The first invite code is kept')
  await home.onPrimaryAction()
  assert.equal(created.length, 0, 'Empty homepage cannot create a trial')
  Object.assign(home.data, { filePath: 'file.pdf', fileName: 'file.pdf', filePreparation: 'ready', engine: 'GPT-6', mode: 'enhanced' })
  home.preparedFile = { path: 'file.pdf', file_token: 'prepared-token', page_count: 12, expires_at: Date.now() / 1000 + 1800 }
  await home.getPaymentConfig()
  assert.equal(home.data.trialKnown, true)
  await Promise.all([home.onPrimaryAction(), home.onPrimaryAction()])
  assert.equal(created.length, 1, 'Double taps cannot consume multiple trials')
  assert.equal(created[0].data.trial, true)
  assert.equal(created[0].data.engine, 'Bing', 'Trial uses standard engine independently of full-document choice')
  assert.equal(created[0].data.ref, 'INVITE23', 'The trial carries the invite code')
  assert.equal(home.data.paymentSheet, false, 'No payment dialog for trial')
  assert.equal(navigated, 1)
  const trialJob = app.globalData.translation
  assert.equal(trialJob.pageCount, 3)
  assert.equal(trialJob.originalPageCount, 12)
  assert.equal(trialJob.fullEngine, 'GPT-6')
  let quotes = 0
  const openPaymentSheet = home.openPaymentSheet
  home.openPaymentSheet = () => { quotes++ }
  await home.onPrimaryAction()
  assert.equal(quotes, 1, 'Returning users must see full-document payment quote')
  home.openPaymentSheet = openPaymentSheet

  // The delivered trial is offered for upgrade on the home page until an upgrade is paid.
  home.data.recent = [trialJob]
  await home.loadOrders()
  assert.equal(home.data.pendingUpgrade.orderID, 'trial-1')
  assert.equal(home.data.recentShort[0].canUpgrade, true)
  assert.equal(home.canUpgrade({ ...trialJob, createdAt: Date.now() - 73 * 3600 * 1000 }), false, 'Trials expire with their server file')
  home.dismissPendingUpgrade()
  assert.equal(home.data.pendingUpgrade, null, 'The banner can be dismissed')
  assert.equal(home.markUpgradable([trialJob])[0].canUpgrade, true, 'Dismissing keeps the recent-list upgrade')
  orders = [{ id: 'paid-0', upgrade_from: 'trial-1', state: 'submitted' }]
  await home.loadOrders()
  assert.equal(home.data.pendingUpgrade, null, 'Upgraded trials leave the banner')
  orders = []

  const preview = load('pages/preview/preview.js')
  preview.onLoad()
  await preview.loadUpgrade()
  assert.equal(preview.data.trial, true)
  assert.equal(preview.data.remainingPages, 9)
  assert.equal(preview.data.upgradeFrom, '¥0.90', 'Cheapest upgrade charges only the untranslated pages')
  preview.translateFull()
  assert.equal(relaunched, 1)
  const reorder = app.globalData.translationReorder
  assert.equal(reorder.upgradeFrom, 'trial-1')
  assert.equal(reorder.engine, 'GPT-6')

  // A regular order offers every engine in the payment sheet and credits nothing.
  home.availableEngines = new Set(['Bing', 'Google', 'DeepSeek-V4.1-Flash', 'GPT-5.6', 'GPT-6'])
  assert.equal(home.upgradeChoices(12).map(c => c.engine).join(), 'Bing,Google,DeepSeek-V4.1-Flash,GPT-5.6,GPT-6')
  assert.equal(home.quoteValues(12, 'Bing').creditPages, 0)
  assert.equal(home.quoteValues(12, 'Bing').paymentTotal, '1.20')

  // Referral pages cover standard pages only; a fully covered order skips WeChat payment.
  home.setData({ creditPages: 5 })
  assert.equal(home.quoteValues(12, 'Bing').referralPages, 5)
  assert.equal(home.quoteValues(12, 'Bing').paymentTotal, '0.70')
  assert.equal(home.quoteValues(12, 'GPT-6').referralPages, 0, 'AI models are not covered')
  home.setData({ creditPages: 20 })
  assert.equal(home.quoteValues(12, 'Bing').paymentTotal, '0.00')
  home.setData({ inviteCode: 'MINE2345' })
  assert.ok(home.onShareAppMessage().path.endsWith('?ref=MINE2345'))
  home.setData({ creditPages: 0 })

  // Upgrade sheet: credit the trial pages, switch engines, and send the trial to the gateway.
  Object.assign(home.data, { engines: ['DeepSeek-V4.1-Flash', 'GPT-5.6', 'GPT-6'], engine: 'GPT-6', mode: 'enhanced', pageCount: 12, sourceIndex: 2, targetIndex: 0 })
  home.availableEngines = new Set(['Bing', 'DeepSeek-V4.1-Flash', 'GPT-5.6', 'GPT-6'])
  home.upgrade = { from: 'trial-1', credit: 3 }
  home.sourceOrderID = 'trial-1'
  const quote = home.quoteValues(12, 'GPT-6')
  assert.equal(quote.fullSubtotal, '4.80')
  assert.equal(quote.creditAmount, '1.20')
  assert.equal(quote.paymentTotal, '3.60')
  const choices = home.upgradeChoices(12)
  assert.equal(choices.map(c => c.engine).join(), 'Bing,DeepSeek-V4.1-Flash,GPT-5.6,GPT-6')
  assert.equal(choices[0].total, '¥0.90')
  home.paymentIntent = { job: home.makeJob(), fileToken: 'trial-file' }
  home.setData({ orderReady: true, paymentSheet: true })
  home.selectUpgradeEngine({ currentTarget: { dataset: { engine: 'DeepSeek-V4.1-Flash' } } })
  assert.equal(home.data.paymentTotal, '2.70')
  assert.equal(home.paymentIntent.job.engine, 'DeepSeek-V4.1-Flash')
  await home.confirmTranslation()
  const paid = created[created.length - 1].data
  assert.equal(paid.upgrade_from, 'trial-1')
  assert.equal(paid.engine, 'DeepSeek-V4.1-Flash')
  // Fully covered by referral pages: no WeChat payment, straight to progress.
  freeOrders = true
  const before = navigated
  home.upgrade = null
  home.setData({ creditPages: 20, engines: ['Bing'], engine: 'Bing', mode: 'standard', paying: false })
  home.setData(home.quoteValues(12, 'Bing'))
  home.paymentIntent = { job: home.makeJob(), fileToken: 'free-file' }
  payment.assertPaymentSupported = () => { throw new Error('free orders must not require WeChat payment') }
  await home.confirmTranslation()
  assert.equal(navigated, before + 1, 'Free order opens the progress page')
  assert.equal(app.globalData.translation.orderID, 'free-1')
  console.log('PASS: three-page trial, upgrade banner, trial result upgrade, engine choice, trial-page and referral credit')
}
run().catch(error => { console.error(error); process.exitCode = 1 })
