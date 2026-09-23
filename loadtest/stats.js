// Load test for GET /{code}/stats. Stats are never cached, so every request
// reads Postgres; this measures the database read path.
import http from 'k6/http';
import { check } from 'k6';
import { BASE_URL, createLinks, deleteLinks, pick, summaryTrendStats } from './lib.js';

const LINKS = Number(__ENV.LINKS || 50);
const RATE = Number(__ENV.RATE || 20);
const DURATION = __ENV.DURATION || '1m';

export const options = {
  scenarios: {
    stats: {
      executor: 'constant-arrival-rate',
      rate: RATE,
      timeUnit: '1s',
      duration: DURATION,
      preAllocatedVUs: 20,
      maxVUs: 500,
    },
  },
  thresholds: {
    'http_req_failed{scenario:stats}': ['rate<0.01'],
  },
  summaryTrendStats,
};

export function setup() {
  return { codes: createLinks(LINKS, false) };
}

export default function ({ codes }) {
  const res = http.get(`${BASE_URL}/${pick(codes)}/stats`, { tags: { name: 'GET /{code}/stats' } });
  check(res, { 'status is 200': (r) => r.status === 200 });
}

export function teardown({ codes }) {
  deleteLinks(codes);
}
