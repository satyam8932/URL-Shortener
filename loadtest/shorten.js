// Load test for link creation: POST /shorten.
//
// Every iteration shortens a new, unique URL, so each request does the full
// write path (idempotency lookup, id reservation, insert). If ADMIN_TOKEN is
// set and CLEANUP=1, the link is deleted again right away; deletes are
// tagged separately but share the database pool, so leave CLEANUP off when
// measuring maximum throughput.
// The API rate limits this endpoint, so run the API with a high limit.
import http from 'k6/http';
import { check } from 'k6';
import { BASE_URL, summaryTrendStats } from './lib.js';

const RATE = Number(__ENV.RATE || 20);
const DURATION = __ENV.DURATION || '1m';
const ADMIN_TOKEN = __ENV.ADMIN_TOKEN || '';
const CLEANUP = __ENV.CLEANUP === '1';

export const options = {
  scenarios: {
    shorten: {
      executor: 'constant-arrival-rate',
      rate: RATE,
      timeUnit: '1s',
      duration: DURATION,
      preAllocatedVUs: 20,
      maxVUs: 500,
    },
  },
  thresholds: {
    'http_req_duration{name:POST /shorten}': ['p(99)<1000'],
    'http_req_failed{name:POST /shorten}': ['rate<0.01'],
  },
  summaryTrendStats,
};

export default function () {
  const url = `https://example.com/loadtest/${Date.now()}/${__VU}/${__ITER}`;
  const res = http.post(`${BASE_URL}/shorten`, JSON.stringify({ url }), {
    headers: { 'Content-Type': 'application/json' },
    tags: { name: 'POST /shorten' },
  });
  check(res, { 'status is 201': (r) => r.status === 201 });

  if (CLEANUP && ADMIN_TOKEN && res.status === 201) {
    http.del(`${BASE_URL}/${res.json('short_code')}`, null, {
      headers: { Authorization: `Bearer ${ADMIN_TOKEN}` },
      tags: { name: 'DELETE /{code}' },
    });
  }
}
