<script lang="ts">
  // How to install a downloaded update (see DownloadUpdate): the
  // explanation, and the command to run, if it's installed by one, in a box
  // with a Copy button, so it's easy to see and doesn't need selecting.
  import { ClipboardSetText } from '../wailsjs/runtime/runtime.js'
  import { ShowDownloadedUpdate } from '../wailsjs/go/main/App.js'

  let { path, hint, command }: { path: string; hint: string; command?: string } = $props()
  let copied = $state(false)
  let box: HTMLDivElement

  // Brought into view when it appears: in Settings it's below the release
  // notes, where nobody would think to scroll.
  $effect(() => {
    box?.scrollIntoView({ block: 'nearest', behavior: 'smooth' })
  })

  async function copy() {
    if (!command) return
    await ClipboardSetText(command)
    copied = true
    setTimeout(() => (copied = false), 2000)
  }
</script>

<div class="install" bind:this={box}>
  <p class="saved">✓ Downloaded and checked against the release signature: <span class="path">{path}</span>
    <button class="link" onclick={() => ShowDownloadedUpdate()}>Show in folder</button></p>
  <p class="hint">{hint}</p>
  {#if command}
    <div class="command">
      <code>{command}</code>
      <button class="primary" onclick={copy}>{copied ? 'Copied ✓' : 'Copy'}</button>
    </div>
  {/if}
</div>

<style>
  .install { flex-basis: 100%; min-width: 0; }
  .saved { margin: 6px 0 2px; color: var(--ok); font-size: 12px; }
  /* Long paths wrap instead of running out of the window. */
  .path, code { overflow-wrap: anywhere; word-break: break-all; }
  .hint { margin: 4px 0; font-weight: 600; }
  .command {
    display: flex; align-items: center; gap: 8px;
    padding: 6px 8px; border-radius: 6px;
    background: var(--bg); border: 1px solid var(--accent);
  }
  code {
    flex: 1; min-width: 0; white-space: pre-wrap;
    font-family: ui-monospace, 'SFMono-Regular', Menlo, Consolas, monospace; font-size: 13px;
    user-select: all;
  }
  button.primary { flex: none; }
</style>
