/**
 * Drive a local `pycage serve` with E2B's own JS/TS SDK.
 *
 *   pycage serve          # listens on 127.0.0.1:49999
 *   npm install && npm start
 *
 * Nothing here talks to e2b.dev.
 */
import { Sandbox, type Execution } from '@e2b/code-interpreter'

const PYCAGE_URL = process.env.PYCAGE_URL ?? 'http://127.0.0.1:49999'

/**
 * Point the E2B SDK at a pycage server.
 *
 * `Sandbox.create()` is deliberately not used: it calls E2B's control plane to
 * allocate a cloud VM, which a local server has no part in. pycage implements
 * the *data* plane — /execute and the /contexts lifecycle — so the sandbox is
 * constructed directly and `sandboxUrl` sends every request to pycage.
 */
function connect(url: string = PYCAGE_URL): Sandbox {
  return new Sandbox({
    sandboxId: 'pycage-local',
    envdVersion: '0.2.0',
    envdAccessToken: process.env.PYCAGE_TOKEN,
    apiKey: 'pycage-local', // unused; the SDK insists on a value
    sandboxUrl: url,
    debug: true,
  })
}

function show(label: string, execution: Execution): void {
  console.log(`\n--- ${label} ---`)
  for (const line of execution.logs.stdout) process.stdout.write(`stdout: ${line}`)
  for (const line of execution.logs.stderr) process.stdout.write(`stderr: ${line}`)
  if (execution.error) {
    console.log(`error : ${execution.error.name}: ${execution.error.value}`)
  }
  if (execution.text !== undefined) console.log('result:', execution.text)
}

async function main(): Promise<void> {
  const sandbox = connect()

  show('compute', await sandbox.runCode('import math\nmath.factorial(20)'))

  // State persists between calls, so an agent can build up work in steps.
  await sandbox.runCode('readings = [3, 1, 4, 1, 5, 9, 2, 6]')
  show(
    'stateful follow-up',
    await sandbox.runCode(
      'from fractions import Fraction\n' +
        'mean = Fraction(sum(readings), len(readings))\n' +
        "print(f'n={len(readings)} mean={float(mean):.3f}')\n" +
        'mean',
    ),
  )

  // A Python exception is data, not a transport failure: the sandbox survives.
  show('recoverable error', await sandbox.runCode('1 / 0'))
  show('still alive', await sandbox.runCode('sum(readings)'))

  // Separate contexts get separate globals and separate filesystems.
  const first = await sandbox.createCodeContext()
  const second = await sandbox.createCodeContext()
  await sandbox.runCode("secret = 'context one only'", { context: first })
  show(
    'isolation',
    await sandbox.runCode("globals().get('secret', 'not visible here')", { context: second }),
  )
}

main().catch((error) => {
  console.error(error)
  process.exit(1)
})
