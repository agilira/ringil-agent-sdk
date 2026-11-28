export async function waitForServer(errorMessage: string) {
  await new Promise((r) => setTimeout(r, 1200));
  for (let i = 0; i < 60; i++) {
    try {
      const r = await fetch("/health");
      if (r.ok) return;
    } catch {
      /* restarting */
    }
    await new Promise((r) => setTimeout(r, 1000));
  }
  throw new Error(errorMessage);
}
