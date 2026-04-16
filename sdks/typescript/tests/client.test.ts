import { describe, it, expect, vi, beforeEach } from 'vitest';
import { Sandbox, AuthError, NotFoundError } from '../src/index';

// Mock the global fetch.
const mockFetch = vi.fn();
vi.stubGlobal('fetch', mockFetch);

function jsonResponse(status: number, body: unknown, ok?: boolean): Response {
  return {
    ok: ok ?? (status >= 200 && status < 300),
    status,
    json: () => Promise.resolve(body),
    headers: new Headers(),
    redirected: false,
    statusText: '',
    type: 'basic',
    url: '',
    clone: () => jsonResponse(status, body, ok),
    body: null,
    bodyUsed: false,
    arrayBuffer: () => Promise.resolve(new ArrayBuffer(0)),
    blob: () => Promise.resolve(new Blob()),
    formData: () => Promise.resolve(new FormData()),
    text: () => Promise.resolve(JSON.stringify(body)),
  } as Response;
}

describe('Sandbox', () => {
  let sandbox: Sandbox;

  beforeEach(() => {
    mockFetch.mockReset();
    sandbox = new Sandbox({ remote: 'http://localhost:19876' });
  });

  describe('create()', () => {
    it('sends correct request and returns VM', async () => {
      mockFetch.mockResolvedValueOnce(
        jsonResponse(201, {
          name: 'test-vm',
          status: 'running',
          guest_ip: '10.0.0.2',
          pid: 1234,
          created_at: '2025-01-01T00:00:00Z',
        }),
      );

      const vm = await sandbox.create('test-vm', { cpus: 2, memoryMb: 512 });

      expect(mockFetch).toHaveBeenCalledTimes(1);
      const [url, init] = mockFetch.mock.calls[0];
      expect(url).toBe('http://localhost:19876/vms');
      expect(init.method).toBe('POST');
      const body = JSON.parse(init.body as string);
      expect(body.name).toBe('test-vm');
      expect(body.cpus).toBe(2);
      expect(body.memory_mb).toBe(512);

      expect(vm.name).toBe('test-vm');
      expect(vm.info.status).toBe('running');
      expect(vm.info.guestIp).toBe('10.0.0.2');
      expect(vm.info.pid).toBe(1234);
    });
  });

  describe('VM.exec()', () => {
    it('returns ExecResult', async () => {
      // First call: create VM.
      mockFetch.mockResolvedValueOnce(
        jsonResponse(201, {
          name: 'test-vm',
          status: 'running',
          guest_ip: '10.0.0.2',
          pid: 1234,
        }),
      );
      const vm = await sandbox.create('test-vm');

      // Second call: exec.
      mockFetch.mockResolvedValueOnce(
        jsonResponse(200, {
          output: 'hello world\n',
          exit_code: 0,
        }),
      );
      const result = await vm.exec('echo hello world');

      expect(result.output).toBe('hello world\n');
      expect(result.exitCode).toBe(0);

      const [url, init] = mockFetch.mock.calls[1];
      expect(url).toBe('http://localhost:19876/vms/test-vm/exec');
      expect(init.method).toBe('POST');
      const body = JSON.parse(init.body as string);
      expect(body.command).toBe('echo hello world');
    });
  });

  describe('list()', () => {
    it('returns array of VMInfo', async () => {
      mockFetch.mockResolvedValueOnce(
        jsonResponse(200, [
          { name: 'vm1', status: 'running', guest_ip: '10.0.0.2', pid: 100 },
          { name: 'vm2', status: 'stopped', pid: 0 },
        ]),
      );

      const vms = await sandbox.list();

      expect(vms).toHaveLength(2);
      expect(vms[0].name).toBe('vm1');
      expect(vms[0].guestIp).toBe('10.0.0.2');
      expect(vms[1].name).toBe('vm2');
      expect(vms[1].status).toBe('stopped');
    });
  });

  describe('auth header', () => {
    it('sends Authorization: Bearer header when apiKey is set', async () => {
      const authedSandbox = new Sandbox({
        remote: 'http://localhost:19876',
        apiKey: 'test-secret-key',
      });

      mockFetch.mockResolvedValueOnce(
        jsonResponse(200, []),
      );

      await authedSandbox.list();

      const [, init] = mockFetch.mock.calls[0];
      expect(init.headers['Authorization']).toBe('Bearer test-secret-key');
    });

    it('does not send Authorization header when apiKey is not set', async () => {
      mockFetch.mockResolvedValueOnce(
        jsonResponse(200, []),
      );

      await sandbox.list();

      const [, init] = mockFetch.mock.calls[0];
      expect(init.headers['Authorization']).toBeUndefined();
    });
  });

  describe('error handling', () => {
    it('throws AuthError on 401', async () => {
      mockFetch.mockResolvedValueOnce(
        jsonResponse(401, { error: 'unauthorized' }, false),
      );

      try {
        await sandbox.list();
        expect.unreachable('should have thrown');
      } catch (e) {
        expect(e).toBeInstanceOf(AuthError);
        expect((e as AuthError).message).toBe('unauthorized');
        expect((e as AuthError).statusCode).toBe(401);
      }
    });

    it('throws NotFoundError on 404', async () => {
      mockFetch.mockResolvedValueOnce(
        jsonResponse(404, { error: 'VM "missing" not found' }, false),
      );

      // We need to create a VM first to call delete on it,
      // but we can also test via deleteImage which hits /images/{name}.
      await expect(sandbox.deleteImage('missing')).rejects.toThrow(NotFoundError);
    });
  });
});
