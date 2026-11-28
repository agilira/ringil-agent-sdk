import { test, expect } from "@playwright/test";
import AxeBuilder from "@axe-core/playwright";
import { readFileSync } from "node:fs";
import { spawnSync } from "node:child_process";
import { join } from "node:path";
test("real chat streaming, tools, approvals and artifact", async ({ page }) => {
  await signin(page);
  const prompt =
    "Use write_file to create acceptance.txt in the current working directory containing exactly hello. Then use create_artifact to make a Markdown artifact named welcome.md containing a short greeting. Call the actual tools; do not just show code.";
  const yes = page.getByRole("button", { name: "Yes", exact: true });
  for (let attempt = 0; attempt < 3; attempt++) {
    await page
      .getByRole("textbox", { name: "Message your agent" })
      .fill(prompt);
    await page.getByRole("button", { name: "Send", exact: true }).click();
    await expect
      .poll(
        async () =>
          (await yes.first().isVisible()) ||
          (await page.locator(".error").count()) > 0,
        { timeout: 180000 },
      )
      .toBeTruthy();
    if (await yes.first().isVisible()) break;
    const error = await page.locator(".error").allTextContents();
    if (
      attempt === 2 ||
      !/429|rate.limit|too many requests/i.test(error.join(" "))
    )
      throw new Error(
        "The real model could not start the acceptance run: " + error.join(" "),
      );
    await page.waitForTimeout(30000);
  }
  await yes.first().click();
  const deadline = Date.now() + 180000;
  while (
    Date.now() < deadline &&
    (await page.getByRole("button", { name: "Stop", exact: true }).isVisible())
  ) {
    if (await yes.first().isVisible()) {
      await Promise.all([
        page.waitForResponse(
          (r) =>
            r.url().includes("/approvals/") && r.request().method() === "POST",
        ),
        yes.first().click(),
      ]);
    }
    await page.waitForTimeout(250);
  }
  await expect(
    page.getByRole("button", { name: "Stop", exact: true }),
  ).toBeHidden({ timeout: 180000 });
  await expect(page.locator(".activity")).toContainText("write_file");
  if (
    !(await page.getByRole("complementary", { name: "Artifacts" }).isVisible())
  )
    await page.getByRole("button", { name: "Artifacts", exact: true }).click();
  await expect(
    page.getByRole("complementary", { name: "Artifacts" }),
  ).toContainText("welcome.md");
  expect((await new AxeBuilder({ page }).analyze()).violations).toEqual([]);
  await page.getByRole("button", { name: "Close", exact: true }).click();
});

test("accessible screens, project handoff and automation", async ({ page }) => {
  await signin(page);
  for (const theme of ["Light", "Dark"]) {
    await page.getByRole("button", { name: "Settings", exact: true }).click();
    await page.getByRole("button", { name: "Appearance", exact: true }).click();
    await page
      .getByRole("combobox", { name: "Theme", exact: true })
      .selectOption(theme.toLowerCase());
    await page.getByRole("button", { name: "Back", exact: true }).click();
    for (const screen of [
      "Connections",
      "Automations",
      "Workflows",
      "Projects",
      "Agents",
    ]) {
      await page
        .getByRole("button", { name: screen, exact: true })
        .first()
        .click();
      await expect(
        page.getByRole("heading", { name: screen, exact: true }),
      ).toBeVisible();
      expect((await new AxeBuilder({ page }).analyze()).violations).toEqual([]);
    }
    await page.getByRole("button", { name: "Settings", exact: true }).click();
    for (const tab of [
      "Models",
      "API keys",
      "Spending limits (USD)",
      "About you / instructions",
      "Voice mode",
      "Memory",
      "Chat apps",
      "Composio",
      "Hooks (advanced)",
      "Export and import",
      "Appearance",
    ]) {
      await page.getByRole("button", { name: tab, exact: true }).click();
      await expect(
        page.getByRole("heading", { name: tab, exact: true }),
      ).toBeVisible();
      expect((await new AxeBuilder({ page }).analyze()).violations).toEqual([]);
    }
    await page.getByRole("button", { name: "Back", exact: true }).click();
    await page.keyboard.press("Tab");
    await expect(page.locator(":focus")).toBeVisible();
  }
  const unique = Date.now().toString(36);
  await page
    .getByRole("button", { name: "Projects", exact: true })
    .first()
    .click();
  await page.getByRole("button", { name: "New project", exact: true }).click();
  await page.getByLabel("Name", { exact: true }).fill("Acceptance " + unique);
  await page
    .getByLabel("Brief", { exact: true })
    .fill("Test the board with a real agent.");
  await page.getByRole("button", { name: "Create", exact: true }).click();
  await page.getByRole("button", { name: "New card", exact: true }).click();
  await page.getByLabel("Title", { exact: true }).fill("Confirm the handoff");
  await page
    .getByLabel("Content", { exact: true })
    .fill("Say hello briefly. Do not call tools.");
  await page
    .getByRole("combobox", { name: "Agent", exact: true })
    .selectOption("default");
  await page.getByRole("button", { name: "Save", exact: true }).click();
  await expect(page.locator(".card")).toContainText("Confirm the handoff");
  await page
    .locator(".card")
    .getByRole("combobox", { name: "Column", exact: true })
    .selectOption("Review");
  await expect(
    page.getByRole("region", { name: "Review", exact: true }).locator(".card"),
  ).toContainText("Confirm the handoff");
  await page.getByRole("tab", { name: "Timeline", exact: true }).click();
  await expect(page.locator(".timeline")).toContainText("run started", {
    timeout: 30000,
  });
  await page
    .locator(".timeline")
    .getByRole("button", { name: "Conversations", exact: true })
    .first()
    .click();
  await settle(page);
  await page
    .getByRole("button", { name: "Projects", exact: true })
    .first()
    .click();
  await page
    .getByRole("button", {
      name: "Acceptance " + unique + " Test the board with a real agent.",
      exact: true,
    })
    .click();
  await page.getByRole("tab", { name: "Timeline", exact: true }).click();
  await expect(page.locator(".timeline")).toContainText("run ended", {
    timeout: 180000,
  });
  expect((await new AxeBuilder({ page }).analyze()).violations).toEqual([]);
  await page
    .getByRole("button", { name: "Automations", exact: true })
    .first()
    .click();
  await page.getByRole("button", { name: "Create", exact: true }).click();
  await page.getByLabel("Name", { exact: true }).fill("acceptance-" + unique);
  await page.getByLabel("Schedule", { exact: true }).fill("@yearly");
  await page
    .getByLabel("What should the agent do?", { exact: true })
    .fill("Reply with a brief greeting. Do not call tools.");
  await page.getByRole("button", { name: "Save", exact: true }).click();
  const item = page.locator("section.surface").filter({
    has: page.getByRole("heading", {
      name: "acceptance-" + unique,
      exact: true,
    }),
  });
  await item.getByRole("button", { name: "Run now", exact: true }).click();
  await expect(
    item.getByRole("button", { name: "Last run", exact: true }),
  ).toBeVisible({ timeout: 180000 });
  await item.getByRole("button", { name: "Last run", exact: true }).click();
  await expect(
    page.getByRole("region", { name: "Last run", exact: true }),
  ).toContainText("Completed", { timeout: 180000 });
  expect((await new AxeBuilder({ page }).analyze()).violations).toEqual([]);
});

async function signin(page: import("@playwright/test").Page) {
  await page.goto("/");
  await expect(page.getByText("Open a new sign-in link")).toBeVisible();
  const state = readFileSync(".e2e-state", "utf8");
  const link = spawnSync(join(state, "ringil"), ["serve", "link"], {
    env: {
      ...process.env,
      XDG_CONFIG_HOME: join(state, "config"),
      XDG_DATA_HOME: join(state, "data"),
    },
    encoding: "utf8",
  });
  const signin = (link.stdout + link.stderr).match(
    /\/signin\?code=[a-f0-9]{64}/,
  )?.[0];
  if (!signin) throw new Error("Could not create sign-in link");
  await page.goto(signin);
  await expect(
    page.getByRole("heading", { name: "What shall we work on?" }),
  ).toBeVisible();
  expect((await new AxeBuilder({ page }).analyze()).violations).toEqual([]);
}

async function settle(page: import("@playwright/test").Page) {
  const stop = page.getByRole("button", { name: "Stop", exact: true });
  // Wait for the run snapshot to attach before observing its final state.
  await expect
    .poll(
      async () =>
        (await stop.isVisible()) ||
        (await page.locator(".message.assistant").count()) > 0,
    )
    .toBeTruthy();
  const end = Date.now() + 150000;
  while (Date.now() < end && (await stop.isVisible())) {
    const approval = page
      .getByRole("button", { name: "Yes", exact: true })
      .first();
    if (await approval.isVisible())
      await Promise.all([
        page.waitForResponse(
          (r) =>
            r.url().includes("/approvals/") && r.request().method() === "POST",
        ),
        approval.click(),
      ]);
    await page.waitForTimeout(250);
  }
  await expect(stop).toBeHidden();
}
