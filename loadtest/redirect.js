// Load test for the redirect hot path: GET /{code}.
//
// setup() creates LINKS links (and visits each once when WARM=1, so the test
// starts with a warm cache). The test then requests them at a fixed RATE per
// second for DURATION, and teardown() deletes them if ADMIN_TOKEN is set.
// See README.md "Performance" for how to run it.
import http from 'k6/http';
import { check } from 'k6';
import { BASE_URL, createLinks, deleteLinks, pick, summaryTrendStats } from './lib.js';

const LINKS = Number(__ENV.LINKS || 50);
const RATE = Number(__ENV.RATE || 200);
const DURATION = __ENV.DURATION || '1m';
const WARM = __ENV.WARM === '1';

export const options = {
  scenarios: {
    redirects: {
      executor: 'constant-arrival-rate',
      rate: RATE,
      timeUnit: '1s',
      duration: DURATION,
      preAllocatedVUs: 50,
      maxVUs: 2000,
    },
  },
  thresholds: {
    'http_req_duration{scenario:redirects}': ['p(99)<50'],
    'http_req_failed{scenario:redirects}': ['rate<0.01'],
  },
  summaryTrendStats,
};

export function setup() {
  return { codes: createLinks(LINKS, WARM) };
}

export default function ({ codes }) {
  const res = http.get(`${BASE_URL}/${pick(codes)}`, { redirects: 0, tags: { name: 'GET /{code}' } });
  check(res, { 'status is 302': (r) => r.status === 302 });
}

export function teardown({ codes }) {
  deleteLinks(codes);
}
