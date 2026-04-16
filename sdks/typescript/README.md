# mvm TypeScript SDK

TypeScript SDK for [mvm](https://github.com/paulmeller/mvm) — local and cloud microVM sandboxes for AI agents.

## Install

```bash
npm install @agentstep/mvm-sdk
```

## Usage

```typescript
import { Sandbox } from '@agentstep/mvm-sdk';

const client = new Sandbox({
  remote: 'https://server:19876',
  apiKey: 'secret',
});

// Create a sandbox
const vm = await client.create('my-sandbox', { cpus: 2, memoryMb: 512 });

// Execute commands
const result = await vm.exec('echo hello');
console.log(result.output);  // "hello\n"

// Checkpoint and restore
await vm.snapshot('before-install');
await vm.exec('npm install express');
await vm.restore('before-install');

// Cleanup
await vm.delete();
```

## Requirements

- Node.js 18+ (for built-in `fetch`)
- A running mvm daemon (local or remote)
