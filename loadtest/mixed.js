// Mixed load test: every kind of traffic at once, each at its own fixed rate,
// with latency reported per kind.
//
//   hot        redirects to popular links, served from Redis        (302)
//   expired    redirects to links the sweeper has expired          (410)
//   unknown    redirects to codes that do not exist                (404)
//   stats      click statistics, always read from Postgres         (200)
//   create     new links, half of them with an expiry              (201)
//   invalid    malformed create requests                           (400)
//
// Rates are per second and can be changed with HOT_RATE, EXPIRED_RATE,
// UNKNOWN_RATE, STATS_RATE, CREATE_RATE and INVALID_RATE.
//
// The API must run with CLIENT_IP_HEADER=X-Client-IP (every create then comes
// from a new client and is never rate limited) and a short
// EXPIRY_SWEEP_INTERVAL. Links made by the create scenario are left in the
// database; remove them with `make loadtest-clean`.
import http from 'k6/http';
import { check, sleep } from 'k6';
import { BASE_URL, deleteLinks, pick, summaryTrendStats } from './lib.js';

const DURATION = __ENV.DURATION || '1m';
const SWEEP_WAIT = Number(__ENV.SWEEP_WAIT || 2);
const IP_HEADER = __ENV.IP_HEADER || 'X-Client-IP';

const rate = (name, fallback) => Number(__ENV[name] || fallback);

function scenario(exec, ratePerSecond) {
  return {
    executor: 'constant-arrival-rate',
    exec,
    rate: ratePerSecond,
    timeUnit: '1s',
    duration: DURATION,
    preAllocatedVUs: Math.max(5, Math.ceil(ratePerSecond / 20)),
    maxVUs: Math.max(50, ratePerSecond),
  };
}

export const options = {
  setupTimeout: '2m',
  scenarios: {
    hot: scenario('hot', rate('HOT_RATE', 1000)),
    expired: scenario('expired', rate('EXPIRED_RATE', 20)),
    unknown: scenario('unknown', rate('UNKNOWN_RATE', 20)),
    stats: scenario('stats', rate('STATS_RATE', 10)),
    create: scenario('create', rate('CREATE_RATE', 5)),
    invalid: scenario('invalid', rate('INVALID_RATE', 5)),
  },
  thresholds: {
    'http_req_duration{scenario:hot}': ['p(99)<50'],
    'http_req_duration{scenario:expired}': ['p(99)<1000'],
    'http_req_duration{scenario:unknown}': ['p(99)<1000'],
    'http_req_duration{scenario:stats}': ['p(99)<1000'],
    'http_req_duration{scenario:create}': ['p(99)<2000'],
    'http_req_duration{scenario:invalid}': ['p(99)<50'],
    checks: ['rate>0.99'],
  },
  summaryTrendStats,
};

function randomIP() {
  const octet = () => Math.floor(Math.random() * 256);
  return `10.${octet()}.${octet()}.${octet()}`;
}

function post(body) {
  return http.post(`${BASE_URL}/shorten`, JSON.stringify(body), {
    headers: { 'Content-Type': 'application/json', [IP_HEADER]: randomIP() },
  });
}

function mustCreate(body) {
  const res = post(body);
  if (res.status !== 201 && res.status !== 200) {
    throw new Error(`setup: POST /shorten returned ${res.status}: ${res.body}`);
  }
  return res.json('short_code');
}

export function setup() {
  const run = Date.now();
  const hot = [];
  const expired = [];

  for (let i = 0; i < 20; i++) {
    const code = mustCreate({ url: `https://example.com/loadtest/mixed/${run}/hot/${i}` });
    http.get(`${BASE_URL}/${code}`, { redirects: 0 });
    hot.push(code);
  }
  for (let i = 0; i < 5; i++) {
    expired.push(mustCreate({ url: `https://example.com/loadtest/mixed/${run}/expired/${i}`, expires_in: 1 }));
  }

  sleep(1 + SWEEP_WAIT);
  for (const code of expired) {
    const res = http.get(`${BASE_URL}/${code}`, { redirects: 0 });
    if (res.status !== 410) {
      throw new Error(`setup: expired link ${code} returned ${res.status}; is EXPIRY_SWEEP_INTERVAL short enough?`);
    }
  }

  return { run, hot, expired };
}

export function hot({ hot }) {
  const res = http.get(`${BASE_URL}/${pick(hot)}`, { redirects: 0 });
  check(res, { 'hot: 302': (r) => r.status === 302 });
}

export function expired({ expired }) {
  const res = http.get(`${BASE_URL}/${pick(expired)}`, { redirects: 0 });
  check(res, { 'expired: 410': (r) => r.status === 410 });
}

// unknown requests a different code every time, so every request misses the
// cache and reaches Postgres. Real codes never start with "zz" at this size.
export function unknown() {
  const code = `zz${(__VU * 1000000 + __ITER).toString(36)}`.slice(0, 10);
  const res = http.get(`${BASE_URL}/${code}`, { redirects: 0 });
  check(res, { 'unknown: 404': (r) => r.status === 404 });
}

export function stats({ hot }) {
  const res = http.get(`${BASE_URL}/${pick(hot)}/stats`);
  check(res, { 'stats: 200': (r) => r.status === 200 });
}

export function create({ run }) {
  const body = { url: `https://example.com/loadtest/mixed/${run}/new/${__VU}/${__ITER}` };
  if (__ITER % 2 === 1) {
    body.expires_in = 3600;
  }
  check(post(body), { 'create: 201': (r) => r.status === 201 });
}

export function invalid() {
  check(post({ url: 'ftp://example.com' }), { 'invalid: 400': (r) => r.status === 400 });
}

export function teardown({ hot, expired }) {
  deleteLinks(hot.concat(expired));
}
