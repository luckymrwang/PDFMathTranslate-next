// node homepage-miniapp.test.cjs /path/to/pdftranslate
const assert = require('node:assert/strict')
const fs = require('node:fs')
const path = require('node:path')
const vm = require('node:vm')
const root = process.argv[2]
assert.ok(root)
async function run() {
  let page, created = [], navigated = 0, relaunched = 0
  let modals = [], modalAnswer = true
  const app = { globalData: {} }, cache = {}
  const realPayment = require(path.join(root, 'utils/payment.js'))
  const payment = { ...realPayment, isIOS: () => false, assertPaymentSupported() {},
    pay: async () => { throw { errMsg: 'requestVirtualPayment:fail cancel' } } }
  const config = { enabled: true, points_enabled: true, points_balance: 0, points_per_page: 10,
    standard_price_fen: 10, enhanced_price_fen: 30,
    engine_prices_fen: { 'GPT-6': 40, 'DeepSeek-V4.1-Flash': 20 },
    engine_points_per_page: { Bing: 10, 'DeepSeek-V4.1-Flash': 10 } }
  let orders = [], freeOrders = false
  const api = {
    getPayConfig: async () => config,
    getReferral: async () => ({ code: 'MINE2345', reward_points: 100, points_balance: config.points_balance || 0 }),
    claimPoints: async () => { config.points_balance = 100; return { points_balance: 100, daily_claimed: true, daily_earned: 100, awarded: 100 } },
    createPreparedOrder: async (token, data) => {
      created.push({ token, data })
      if (data.trial) return { order: { id: 'trial-1', engine: 'Bing', pages: 3 } }
      return freeOrders ? { order: { id: 'free-1', state: 'paid', total_fen: 0, points_spent: 120, credit_pages: 12 } } : { order: { id: 'paid-1', total_fen: 270 }, payData: {} }
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
      if (name.endsWith('/rewarded-ad')) return vm.runInNewContext(
        fs.readFileSync(path.join(root, 'utils/rewarded-ad.js'), 'utf8') + ';module.exports',
        { module: { exports: {} }, wx: sandbox.wx, console })
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
      showModal: options => { modals.push(options); if (options.success) options.success({ confirm: modalAnswer, cancel: !modalAnswer }) },
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
  home.availableEngines = new Set(['Bing', 'GPT-6'])
  await Promise.all([home.onPrimaryAction(), home.onPrimaryAction()])
  assert.equal(created.length, 0, 'Opening a quote never creates an order or free trial')
  assert.equal(home.data.paymentSheet, true)
  assert.equal(home.data.trialAvailable, false)
  const trialJob = { name: 'old.pdf', orderID: 'trial-1', pageCount: 3, originalPageCount: 12,
    trial: true, engine: 'Bing', fullEngine: 'GPT-6', createdAt: Date.now() }
  app.globalData.translation = trialJob
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
  await preview.loadInvitation()
  // onLoad already started loading; let the shared request finish.
  await new Promise(resolve => setImmediate(resolve))
  assert.equal(preview.data.inviteCode, 'MINE2345')
  assert.equal(preview.data.rewardPoints, 100)
  const resultShare = preview.onShareAppMessage()
  assert.equal(resultShare.path, '/pages/index/index?ref=MINE2345')
  assert.equal(resultShare.imageUrl, '/assets/share-card.jpg')
  assert.ok(!JSON.stringify(resultShare).includes(app.globalData.translation.name), 'Share must not expose the document name')
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
  assert.equal(home.quoteValues(12, 'Bing').trialCreditPages, 0)
  assert.equal(home.quoteValues(12, 'Bing').paymentTotal, '1.20')

  // Claims and quote updates must not overwrite wallet balances.
  let claimCalls = 0
  const claim = api.claimPoints
  api.claimPoints = async () => { claimCalls++; return claim() }
  let resolveOldWallet
  const getReferral = api.getReferral
  api.getReferral = () => new Promise(resolve => { resolveOldWallet = resolve })
  const oldWalletRead = home.loadWallet()
  await Promise.all([home.claimDailyPoints(), home.claimDailyPoints()])
  resolveOldWallet({ points_balance: 0, daily_claimed: false })
  await oldWalletRead
  api.getReferral = getReferral
  assert.equal(claimCalls, 1, 'Double taps only send one daily claim')
  assert.equal(home.data.pointsBalance, 100)
  assert.equal(home.data.dailyClaimed, true)
  home.setData(home.quoteValues(12, 'Bing'))
  home.setData(home.quoteValues(12, 'Bing'))
  assert.equal(home.data.pointsBalance, 100)
  assert.equal(home.data.pointsCoveredPages, 10)
  assert.equal(home.data.paymentTotal, '0.20')
  assert.equal(home.quoteValues(12, 'Google').pointsCoveredPages, 0)
  assert.equal(home.quoteValues(12, 'GPT-6').pointsCoveredPages, 0)
  // DeepSeek redeems 10 points per page like Bing; uncovered pages cost 0.2 yuan.
  const deepSeek = home.quoteValues(12, 'DeepSeek-V4.1-Flash')
  assert.equal(deepSeek.pointsCoveredPages, 10)
  assert.equal(deepSeek.pointsSpent, 100)
  assert.equal(deepSeek.pointsAmount, '2.00')
  assert.equal(deepSeek.paymentTotal, '0.40')
  assert.equal(home.quoteValues(12, 'DeepSeek-V4.1-Flash').unitPrice, '0.20')
  home.setData({ pointsBalance: 9 })
  assert.equal(home.quoteValues(12, 'Bing').pointsCoveredPages, 0)
  home.setData({ pointsBalance: 19 })
  assert.equal(home.quoteValues(12, 'Bing').pointsSpent, 10)
  home.setData({ pointsBalance: 200, inviteCode: 'MINE2345' })
  assert.equal(home.quoteValues(12, 'Bing').paymentTotal, '0.00')
  // Fully points-covered engines quote points; cash-only and partly covered ones keep yuan.
  home.availableEngines = new Set(['Bing', 'DeepSeek-V4.1-Flash', 'GPT-6'])
  const labels = Object.fromEntries(home.upgradeChoices(12).map(c => [c.engine, c.price + '|' + c.total]))
  assert.equal(labels.Bing, '10 积分/页|120 积分')
  assert.equal(labels['DeepSeek-V4.1-Flash'], '10 积分/页|120 积分')
  assert.equal(labels['GPT-6'], '0.4 元/页|¥4.80')
  assert.equal(Object.fromEntries(home.upgradeChoices(30).map(c => [c.engine, c.price]))['DeepSeek-V4.1-Flash'], '0.2 元/页')
  assert.ok(home.onShareAppMessage().path.endsWith('?ref=MINE2345'))
  assert.ok(home.onShareAppMessage().title.includes('100 积分'))
  home.setData({ pointsBalance: 0 })

  home.setData({ paymentSheet: true, pageCount: 10, engine: 'Bing' })
  home.applyWallet({ points_balance: 100 })
  assert.equal(home.data.paymentTotal, '0.00', 'Wallet refresh updates an open uncreated quote')
  home.applyWallet({ points_balance: 0 })

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
  assert.equal(home.data.paymentTotal, '1.80')
  assert.equal(home.paymentIntent.job.engine, 'DeepSeek-V4.1-Flash')
  await home.confirmTranslation()
  const paid = created[created.length - 1].data
  assert.equal(paid.upgrade_from, 'trial-1')
  assert.equal(paid.engine, 'DeepSeek-V4.1-Flash')
  assert.equal(paid.output, 'dual', 'Orders default to the dual PDF')
  const cancelledCreations = created.length
  await home.confirmTranslation()
  assert.equal(created.length, cancelledCreations + 1, 'Cancelled payment retries create a fresh order')
  // Scanned PDF: no modal; the payment sheet flags only engines that cannot read images.
  home.setData({ scanEngines: ['GPT-5.6', 'GPT-6'] })
  const scanNote = engine => { const v = home.scanValues(engine); return v.scanWarn + ':' + v.scanTarget }
  assert.equal(scanNote('Bing'), 'true:GPT-5.6', 'Text-only engine is flagged')
  assert.equal(scanNote('GPT-6'), 'false:', 'Image-capable engine is not flagged')
  // Paying for a scan with a text-only engine asks first and can switch the engine.
  home.setData({ engines: ['Bing', 'GPT-5.6', 'GPT-6'], engine: 'Bing',
    scanWarn: true, scanTarget: 'GPT-5.6', scanNames: 'GPT-5.6 或 GPT-6' })
  modalAnswer = true
  assert.equal(await home.scanReadyToPay(), false, 'Choosing the scan engine stops this payment')
  assert.equal(home.data.engine, 'GPT-5.6', 'and selects it')
  assert.equal(modals.length, 1)
  assert.match(modals[0].content, /GPT-5\.6/)
  home.setData({ engine: 'Bing', scanWarn: true, scanTarget: 'GPT-5.6', scanNames: 'GPT-5.6 或 GPT-6' })
  modalAnswer = false
  assert.equal(await home.scanReadyToPay(), true, 'The user may still pay with a text-only engine')
  assert.equal(await home.scanReadyToPay(), true, 'and is asked only once for that engine')
  assert.equal(modals.length, 2)
  // Paying for a scan with a text-only engine asks first and can switch the engine.
  home.setData({ engines: ['Bing', 'GPT-5.6', 'GPT-6'], engine: 'Bing', paymentTotal: '0.00',
    scanWarn: true, scanTarget: 'GPT-5.6', scanNames: 'GPT-5.6 或 GPT-6',
    upgradeOptions: [{ engine: 'GPT-5.6', scanOk: true }] })
  let ready = home.scanReadyToPay()
  assert.equal(home.data.scanDialog, true, 'A scan on a text-only engine opens the check')
  assert.match(home.data.scanOutcome, /积分自动退回/)
  home.scanDialogSwitch({ currentTarget: { dataset: { engine: 'GPT-5.6' } } })
  assert.equal(await ready, false, 'Choosing the scan engine stops this payment')
  assert.equal(home.data.engine, 'GPT-5.6', 'and selects it')
  assert.equal(home.data.scanDialog, false)
  home.setData({ engine: 'Bing', scanWarn: true, scanTarget: 'GPT-5.6', scanNames: 'GPT-5.6 或 GPT-6' })
  ready = home.scanReadyToPay()
  home.scanDialogClose()
  assert.equal(await ready, false, 'Closing the check does not pay')
  ready = home.scanReadyToPay()
  home.scanDialogContinue()
  assert.equal(await ready, true, 'The user may still pay with a text-only engine')
  assert.equal(await home.scanReadyToPay(), true, 'and is asked only once for that engine')
  // First scanned order: GPT quotes leave the first 3 pages out once the gateway offers it.
  home.setData({ scanFreeOffer: 0 })
  const sevenPages = home.quoteValues(7, 'GPT-5.6').paymentTotal
  home.setData({ scanFreeOffer: 3 })
  const discounted = home.quoteValues(10, 'GPT-5.6')
  assert.equal(discounted.scanFreePages, 3, 'GPT quote shows the free pages')
  assert.equal(discounted.paymentTotal, sevenPages, '10 scanned pages cost the same as 7')
  assert.equal(home.quoteValues(3, 'GPT-5.6').scanFreePages, 0, 'Three pages or fewer are not discounted')
  assert.equal(home.quoteValues(10, 'Bing').scanFreePages, 0, 'Text-only engines are not discounted')
  home.setData({ scanEngines: [], scanFreeOffer: 0 })
  assert.equal(home.scanValues('Bing').scanWarn, false, 'Text PDFs show no scan note')
  assert.equal(home.quoteValues(10, 'GPT-5.6').scanFreePages, 0, 'Text PDFs are not discounted')
  // Output format: remembered, carried by the job, unknown values fall back to dual.
  home.selectOutput({ currentTarget: { dataset: { value: 'both' } } })
  assert.equal(home.data.output, 'both')
  assert.equal(home.makeJob().output, 'both')
  assert.ok(Object.values(cache).includes('both'), 'Output choice is remembered')
  home.selectOutput({ currentTarget: { dataset: { value: 'html' } } })
  assert.equal(home.data.output, 'dual')
  // Fully covered by referral pages: no WeChat payment, straight to progress.
  freeOrders = true
  const before = navigated
  home.upgrade = null
  home.setData({ pointsBalance: 200, engines: ['Bing'], engine: 'Bing', mode: 'standard', paying: false })
  home.setData(home.quoteValues(12, 'Bing'))
  home.paymentIntent = { job: home.makeJob(), fileToken: 'free-file' }
  payment.assertPaymentSupported = () => { throw new Error('free orders must not require WeChat payment') }
  await home.confirmTranslation()
  assert.equal(navigated, before + 1, 'Free order opens the progress page')
  assert.equal(app.globalData.translation.orderID, 'free-1')
  // Lazy login carries the environment-scoped invitation on the first request.
  let loginBody
  const apiModule = { exports: {} }
  vm.runInNewContext(fs.readFileSync(path.join(root, 'utils/api.js'), 'utf8'), {
    module: apiModule, require: name => name === './storage-key' ? { storageKey: k => 'dev:' + k } : { baseUrl: 'https://dev.2026669.me' },
    wx: { getStorageSync: k => cache[k], setStorageSync: (k, value) => { cache[k] = value },
      login: ({ success }) => success({ code: 'wx-code' }), request: options => {
        loginBody = options.data; options.success({ statusCode: 200, data: { token: 'new-token' } })
      } },
  })
  await apiModule.exports.login()
  assert.equal(loginBody.ref, 'INVITE23')
  assert.equal(cache['dev:gw_token'], 'new-token')
  console.log('PASS: points claim, Bing/DeepSeek quotes, wallet stability, historical upgrades and points-only orders')
}
run().catch(error => { console.error(error); process.exitCode = 1 })
