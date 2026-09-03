import { test } from '@playwright/test';
import * as fs from 'fs';
test('add GST rate', async ({ page }) => {
  const pw = /password=(.+)/.exec(fs.readFileSync('C:/ProgramData/ISP BSS/config/initial_admin.txt','utf8'))![1].trim();
  await page.goto('/staff/login');
  await page.fill('#username','admin'); await page.fill('#password',pw);
  await page.click('button[type="submit"]'); await page.waitForLoadState('networkidle');
  await page.goto('/staff/catalogue');
  const f = page.locator('form[action="/staff/catalogue/gst"]');
  await f.locator('input[name="cgst_rate"]').fill('9');
  await f.locator('input[name="sgst_rate"]').fill('9');
  await f.locator('input[name="igst_rate"]').fill('18');
  await f.locator('button[type="submit"]').click();
  await page.waitForLoadState('networkidle');
  const b = await page.locator('body').innerText();
  console.log('RESULT=' + b.slice(b.indexOf('GST RATES'), b.indexOf('GST RATES')+220).replace(/\s+/g,' '));
});
