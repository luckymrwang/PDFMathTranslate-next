// Run: node environment-miniapp.test.cjs /absolute/path/to/pdftranslate
const assert = require('node:assert/strict')
const fs = require('node:fs')
const path = require('node:path')
const vm = require('node:vm')
const root = process.argv[2]
assert.ok(root, 'Provide the miniapp directory')

function loadConfig(envVersion) {
  const module = { exports: {} }
  vm.runInNewContext(fs.readFileSync(path.join(root, 'config.js'), 'utf8'), {
    module,
    wx: { getAccountInfoSync: () => ({ miniProgram: { envVersion } }) },
  })
  return module.exports
}

const keys = ['gw_token', 'translation_history', 'virtualpay_pending_jobs']
const configs = {}
for (const env of ['develop', 'trial', 'release']) {
  const config = loadConfig(env)
  configs[env] = config
  assert.equal(config.baseUrl, env === 'release' ? 'https://2026669.me' : 'https://dev.2026669.me')
  assert.equal(config.demoMode, false)
  const module = { exports: {} }
  vm.runInNewContext(fs.readFileSync(path.join(root, 'utils/storage-key.js'), 'utf8'), {
    module,
    require: name => { assert.equal(name, '../config'); return config },
  })
  for (const key of keys) {
    assert.equal(module.exports.storageKey(key), env === 'release' ? key : `dev:${key}`)
  }
}
assert.equal(configs.develop.storagePrefix, configs.trial.storagePrefix)
assert.throws(() => loadConfig('unknown'), /未知/)

// Ensure all existing token, history and pending-order reads/writes are scoped.
for (const file of ['utils/api.js', 'utils/order-status.js', 'pages/index/index.js',
  'pages/history/history.js', 'pages/progress/progress.js']) {
  const source = fs.readFileSync(path.join(root, file), 'utf8')
  assert.doesNotMatch(source, /wx\.(?:get|set)StorageSync\(['"](?:gw_token|translation_history|virtualpay_pending_jobs)['"]/, file)
}
console.log('Miniapp environment and storage isolation checks passed')
