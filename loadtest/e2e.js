// End-to-end check of every endpoint, status code and edge case against a
// running API. Fails if any single check fails.
//
// The API must run with CLIENT_IP_HEADER=X-Client-IP, so this script can act
// as many different clients, and with a short EXPIRY_SWEEP_INTERVAL (1s), so
// expiry can be observed quickly. See README.md "End-to-end check".
import http from 'k6/http';
import { check, group, sleep } from 'k6';
import { BASE_URL } from './lib.js';

const ADMIN_TOKEN = __ENV.ADMIN_TOKEN || '';
const IP_HEADER = __ENV.IP_HEADER || 'X-Client-IP';
const BURST = Number(__ENV.BURST || 5);
const SWEEP_WAIT = Number(__ENV.SWEEP_WAIT || 2);
const MAX_URL_LENGTH = 2048;
const MAX_BODY_BYTES = 8 << 10;

export const options = {
  vus: 1,
  iterations: 1,
  thresholds: { checks: ['rate==1'] },
};

let ipCounter = 0;

// freshIP returns an address no earlier request used, so the rate limiter
// only affects the group that tests it.
function freshIP() {
  ipCounter++;
  return `10.${(ipCounter >> 16) & 255}.${(ipCounter >> 8) & 255}.${ipCounter & 255}`;
}

function post(body, ip = freshIP()) {
  const payload = typeof body === 'string' ? body : JSON.stringify(body);
  return http.post(`${BASE_URL}/shorten`, payload, {
    headers: { 'Content-Type': 'application/json', [IP_HEADER]: ip },
  });
}

function request(method, path) {
  return http.request(method, `${BASE_URL}${path}`, null, { redirects: 0 });
}

function remove(code, authorization) {
  const headers = authorization ? { Authorization: authorization } : {};
  return http.del(`${BASE_URL}/${code}`, null, { headers });
}

function stats(code) {
  return request('GET', `/${code}/stats`);
}

const status = (want) => (r) => r.status === want;

export default function () {
  if (!ADMIN_TOKEN) {
    throw new Error('ADMIN_TOKEN is required');
  }

  const run = String(Date.now());
  const url = `https://example.com/loadtest/e2e/${run}`;
  const cleanup = [];

  group('health', () => {
    check(request('GET', '/healthz'), { 'healthz is 200': status(200) });
    check(request('GET', '/readyz'), { 'readyz is 200': status(200) });
  });

  group('create', () => {
    const first = post({ url });
    check(first, {
      'new link is 201': status(201),
      'short_url ends with the code': (r) => r.json('short_url').endsWith(`/${r.json('short_code')}`),
      'permanent link has no expires_at': (r) => r.json('expires_at') === undefined,
    });
    cleanup.push(first.json('short_code'));

    check(post({ url }), {
      'same url again is 200': status(200),
      'same url returns the same code': (r) => r.json('short_code') === first.json('short_code'),
    });

    const expiring = post({ url, expires_in: 3600 });
    check(expiring, {
      'same url with expiry is a new 201 link': (r) => r.status === 201 && r.json('short_code') !== first.json('short_code'),
      'expiring link has expires_at': (r) => typeof r.json('expires_at') === 'string',
    });
    cleanup.push(expiring.json('short_code'));
  });

  group('aliases', () => {
    const alias = `e${run.slice(-8)}`;
    const created = post({ url: `${url}/alias`, alias });
    check(created, {
      'alias is 201': status(201),
      'alias is used as the code': (r) => r.json('short_code') === alias,
    });
    cleanup.push(alias);

    check(post({ url: `${url}/other`, alias }), { 'taken alias is 409': status(409) });
    check(post({ url, alias: 'healthz' }), { 'reserved alias is 400': status(400) });
    check(post({ url, alias: 'ab' }), { 'too short alias is 400': status(400) });
    check(post({ url, alias: 'abcdefghijk' }), { 'too long alias is 400': status(400) });
    check(post({ url, alias: 'a/b.c' }), { 'alias with bad characters is 400': status(400) });
  });

  group('input validation', () => {
    const invalid = [
      ['missing url', {}],
      ['empty url', { url: '' }],
      ['relative url', { url: '/just/a/path' }],
      ['ftp url', { url: 'ftp://example.com/file' }],
      ['javascript url', { url: 'javascript:alert(1)' }],
      ['url without host', { url: 'https://' }],
      ['url too long', { url: `https://example.com/${'a'.repeat(MAX_URL_LENGTH)}` }],
      ['unknown field', { url, slug: 'x' }],
      ['expires_in zero', { url, expires_in: 0 }],
      ['expires_in negative', { url, expires_in: -5 }],
      ['expires_in over one year', { url, expires_in: 31536001 }],
      ['expires_in that overflows', { url, expires_in: 18446744074 }],
      ['expires_in as string', { url, expires_in: '60' }],
      ['not json', 'url=https://example.com'],
      ['json array', '[]'],
    ];
    for (const [name, body] of invalid) {
      check(post(body), { [`${name} is 400`]: status(400) });
    }
    check(post(`{"url":"https://example.com/${'a'.repeat(MAX_BODY_BYTES)}"}`), { 'oversized body is 413': status(413) });
    check(post(''), { 'empty body is 400': status(400) });
  });

  group('redirect', () => {
    const code = post({ url: `${url}/redirect` }).json('short_code');
    cleanup.push(code);

    check(request('GET', `/${code}`), {
      'redirect is 302 (not 301)': status(302),
      'redirect has the original Location': (r) => r.headers.Location === `${url}/redirect`,
    });
    check(request('GET', `/${code}`), { 'second (cached) redirect is 302': status(302) });
    check(request('HEAD', `/${code}`), { 'HEAD redirect is 302': status(302) });
    check(request('GET', `/zz${run.slice(-8)}`), { 'unknown code is 404': status(404) });
    check(request('GET', '/favicon.ico'), { 'invalid code is 404': status(404) });
    check(request('GET', '/abcdefghijk'), { 'code longer than 10 is 404': status(404) });
    check(request('GET', `/${code}/extra/path`), { 'nested path is 404': status(404) });
    check(request('PUT', `/${code}`), { 'wrong method is 405': status(405) });
  });

  group('click counting', () => {
    const code = post({ url: `${url}/clicks` }).json('short_code');
    cleanup.push(code);

    check(stats(code), { 'new link has 0 clicks': (r) => r.json('click_count') === 0 });
    for (let i = 0; i < 3; i++) {
      request('GET', `/${code}`);
    }
    request('HEAD', `/${code}`);
    sleep(1);
    check(stats(code), { 'three GETs count as 3 clicks, HEAD does not count': (r) => r.json('click_count') === 3 });
  });

  group('stats', () => {
    const code = post({ url: `${url}/stats` }).json('short_code');
    cleanup.push(code);

    check(stats(code), {
      'stats is 200': status(200),
      'stats has the original url': (r) => r.json('original_url') === `${url}/stats`,
      'stats has created_at': (r) => typeof r.json('created_at') === 'string',
      'stats shows not expired': (r) => r.json('expired') === false,
    });
    check(stats(`zz${run.slice(-8)}`), { 'stats for unknown code is 404': status(404) });
    check(stats('bad.code'), { 'stats for invalid code is 404': status(404) });
  });

  group('expiry', () => {
    const code = post({ url: `${url}/expiry`, expires_in: 2 }).json('short_code');
    cleanup.push(code);

    check(request('GET', `/${code}`), { 'link redirects before it expires': status(302) });
    sleep(2 + SWEEP_WAIT);
    check(request('GET', `/${code}`), { 'expired link is 410, not served from cache': status(410) });
    check(stats(code), {
      'stats still works for an expired link': status(200),
      'stats shows expired': (r) => r.json('expired') === true,
    });

    const permanent = post({ url: `${url}/expiry` });
    check(permanent, { 'permanent link for an expired url is a new link': (r) => r.status === 201 && r.json('short_code') !== code });
    cleanup.push(permanent.json('short_code'));
  });

  group('delete', () => {
    const code = post({ url: `${url}/delete` }).json('short_code');
    request('GET', `/${code}`);

    check(remove(code), {
      'delete without token is 401': status(401),
      '401 sets WWW-Authenticate': (r) => r.headers['Www-Authenticate'] === 'Bearer',
    });
    check(remove(code, 'Bearer wrong-token'), { 'delete with wrong token is 401': status(401) });
    check(remove(code, `Basic ${ADMIN_TOKEN}`), { 'delete with wrong scheme is 401': status(401) });
    check(remove(code, `bearer ${ADMIN_TOKEN}`), { 'delete with lowercase bearer is 204': status(204) });
    check(remove(code, `Bearer ${ADMIN_TOKEN}`), { 'deleting again is 404': status(404) });
    check(request('GET', `/${code}`), { 'deleted link is 404, not served from cache': status(404) });
    check(stats(code), { 'stats for deleted link is 404': status(404) });
  });

  group('rate limit', () => {
    const block = 1 + Math.floor(Math.random() * 250);
    const ip = `198.51.100.${block}`;
    for (let i = 0; i < BURST; i++) {
      check(post({}, ip), { 'requests within burst are not limited': (r) => r.status !== 429 });
    }
    const limited = post({}, ip);
    check(limited, {
      'request over burst is 429': status(429),
      '429 has Retry-After in seconds': (r) => Number(r.headers['Retry-After']) >= 1,
    });
    check(post({}), { 'another client is not limited': (r) => r.status !== 429 });

    const hex = block.toString(16);
    for (let i = 0; i < BURST; i++) {
      post({}, `2001:db8:${hex}:1::${i + 1}`);
    }
    check(post({}, `2001:db8:${hex}:1::ffff`), { 'ipv6 addresses in the same /64 share a limit': status(429) });
    check(post({}, `2001:db8:${hex}:2::1`), { 'a different ipv6 /64 is not limited': (r) => r.status !== 429 });
  });

  for (const code of cleanup) {
    remove(code, `Bearer ${ADMIN_TOKEN}`);
  }
}
