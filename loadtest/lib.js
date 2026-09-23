// Helpers shared by the load test scripts.
import http from 'k6/http';

export const BASE_URL = __ENV.BASE_URL || 'http://localhost:8080';
const ADMIN_TOKEN = __ENV.ADMIN_TOKEN || '';

// createLinks shortens count unique URLs and returns their codes. With warm
// set, it visits each link once so the test starts with a warm cache.
export function createLinks(count, warm) {
  const run = Date.now();
  const codes = [];
  for (let i = 0; i < count; i++) {
    const res = http.post(
      `${BASE_URL}/shorten`,
      JSON.stringify({ url: `https://example.com/loadtest/${run}/${i}` }),
      { headers: { 'Content-Type': 'application/json' } },
    );
    if (res.status !== 201 && res.status !== 200) {
      throw new Error(`setup: POST /shorten returned ${res.status}: ${res.body}`);
    }
    const code = res.json('short_code');
    codes.push(code);
    if (warm) {
      http.get(`${BASE_URL}/${code}`, { redirects: 0 });
    }
  }
  return codes;
}

// deleteLinks removes the given links, or warns that they are left behind
// when no ADMIN_TOKEN is available.
export function deleteLinks(codes) {
  if (!ADMIN_TOKEN) {
    console.warn(`ADMIN_TOKEN not set: ${codes.length} test links left in the database`);
    return;
  }
  for (const code of codes) {
    http.del(`${BASE_URL}/${code}`, null, { headers: { Authorization: `Bearer ${ADMIN_TOKEN}` } });
  }
}

export function pick(codes) {
  return codes[Math.floor(Math.random() * codes.length)];
}

export const summaryTrendStats = ['min', 'med', 'p(90)', 'p(95)', 'p(99)', 'max'];
