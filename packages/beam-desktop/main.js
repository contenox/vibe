const { app, BrowserWindow, dialog } = require('electron')
const { spawn, spawnSync } = require('child_process')
const path = require('path')
const http = require('http')
const os = require('os')

const isDev = process.argv.includes('--dev')

const binaryPath = isDev
  ? path.join(__dirname, '../../bin/contenox')
  : path.join(process.resourcesPath, 'contenox')

const dataDir = path.join(os.homedir(), '.contenox')

// If called with arguments, run the contenox binary as a CLI and exit.
const cliArgs = process.argv.slice(1).filter(arg =>
  arg !== '--dev' &&
  arg !== '--no-sandbox' &&
  !arg.startsWith('--inspect') &&
  !arg.startsWith('--remote-debugging-port=')
)

if (cliArgs.length > 0) {
  const result = spawnSync(binaryPath, cliArgs, { stdio: 'inherit' })
  process.exit(result.status ?? 1)
}

// No arguments — open Beam UI.
let serverProcess = null

function startServer() {
  return new Promise((resolve, reject) => {
    serverProcess = spawn(binaryPath, ['beam', '--data-dir', dataDir], {
      stdio: ['ignore', 'pipe', 'pipe'],
    })

    serverProcess.stdout.on('data', (d) => {
      if (d.toString().includes('Beam ready')) resolve()
    })
    serverProcess.stderr.on('data', (d) => process.stderr.write(d))
    serverProcess.on('error', reject)
    serverProcess.on('exit', (code) => {
      if (code !== 0 && code !== null) reject(new Error(`server exited with code ${code}`))
    })

    const poll = setInterval(() => {
      http.get('http://127.0.0.1:8081/api/health', (res) => {
        if (res.statusCode === 200) { clearInterval(poll); resolve() }
      }).on('error', () => {})
    }, 500)

    setTimeout(() => {
      clearInterval(poll)
      reject(new Error('server did not start within 30s'))
    }, 30000)
  })
}

app.whenReady().then(async () => {
  try {
    await startServer()
  } catch (err) {
    dialog.showErrorBox('Contenox Beam', `Failed to start server:\n${err.message}`)
    app.quit()
    return
  }

  const win = new BrowserWindow({
    width: 1280,
    height: 800,
    title: 'Contenox Beam',
    webPreferences: { nodeIntegration: false, contextIsolation: true },
  })

  win.loadURL('http://127.0.0.1:8081')
  if (isDev) win.webContents.openDevTools()
})

app.on('window-all-closed', () => {
  if (serverProcess) serverProcess.kill()
  app.quit()
})
