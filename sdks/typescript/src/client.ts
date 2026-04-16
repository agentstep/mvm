import {
  SandboxOptions,
  CreateVMOptions,
  VMInfo,
  ExecResult,
  BuildStep,
  ImageInfo,
  SnapshotInfo,
  PoolStatus,
  PortMap,
} from './types';
import {
  MVMError,
  AuthError,
  NotFoundError,
  ConflictError,
  ServerError,
} from './errors';

/**
 * VM represents a single virtual machine returned by Sandbox.create().
 * It provides methods to interact with the VM (exec, stop, pause, etc.).
 */
export class VM {
  constructor(
    private client: Sandbox,
    public name: string,
    public info: VMInfo,
  ) {}

  /** Execute a command inside the VM. */
  async exec(command: string): Promise<ExecResult> {
    const resp = await this.client.request<{ output: string; exit_code: number }>(
      'POST',
      `/vms/${this.name}/exec`,
      { command },
    );
    return { output: resp.output ?? '', exitCode: resp.exit_code };
  }

  /** Stop the VM. If force is true, the VM is killed immediately. */
  async stop(force?: boolean): Promise<void> {
    await this.client.request('POST', `/vms/${this.name}/stop`, { force: force ?? false });
  }

  /** Delete the VM. If force is true, the VM is stopped first if running. */
  async delete(force?: boolean): Promise<void> {
    if (force) {
      try {
        await this.stop(true);
      } catch {
        // Ignore stop errors when force-deleting.
      }
    }
    await this.client.request('DELETE', `/vms/${this.name}`);
  }

  /** Pause a running VM. */
  async pause(): Promise<void> {
    await this.client.request('POST', `/vms/${this.name}/pause`);
  }

  /** Resume a paused VM. */
  async resume(): Promise<void> {
    await this.client.request('POST', `/vms/${this.name}/resume`);
  }

  /** Create a snapshot of this VM. */
  async snapshot(name: string): Promise<void> {
    await this.client.request('POST', `/vms/${this.name}/snapshot`, { name });
  }

  /** Restore this VM from a named snapshot. */
  async restore(snapshotName: string): Promise<void> {
    await this.client.request('POST', `/vms/${this.name}/restore`, { name: snapshotName });
  }
}

/**
 * Sandbox is the top-level client for the mvm daemon API.
 * It manages VMs, images, snapshots, and the warm pool.
 */
export class Sandbox {
  private baseURL: string;
  private apiKey?: string;

  constructor(options: SandboxOptions) {
    this.baseURL = options.remote.replace(/\/+$/, '');
    this.apiKey = options.apiKey;
  }

  /**
   * Send an HTTP request to the daemon.
   * Handles JSON serialization, auth headers, and error mapping.
   * @internal Exposed for VM to call; not part of the public API contract.
   */
  async request<T = void>(method: string, path: string, body?: unknown): Promise<T> {
    const url = this.baseURL + path;
    const headers: Record<string, string> = {};

    if (this.apiKey) {
      headers['Authorization'] = `Bearer ${this.apiKey}`;
    }

    const init: RequestInit = { method, headers };

    if (body !== undefined) {
      headers['Content-Type'] = 'application/json';
      init.body = JSON.stringify(body);
    }

    const resp = await fetch(url, init);

    if (!resp.ok) {
      let message = `mvm api ${resp.status}`;
      try {
        const errBody = (await resp.json()) as { error?: string };
        if (errBody.error) {
          message = errBody.error;
        }
      } catch {
        // Response body was not JSON; use default message.
      }
      throw this.mapError(resp.status, message);
    }

    // 204 No Content — nothing to parse.
    if (resp.status === 204) {
      return undefined as T;
    }

    return (await resp.json()) as T;
  }

  /** Map an HTTP status code to a typed error. */
  private mapError(status: number, message: string): MVMError {
    switch (status) {
      case 401:
        return new AuthError(message);
      case 404:
        return new NotFoundError(message);
      case 409:
        return new ConflictError(message);
      default:
        if (status >= 500) {
          return new ServerError(message, status);
        }
        return new MVMError(message, status);
    }
  }

  /** Create a new VM and return a VM handle. */
  async create(name: string, options?: CreateVMOptions): Promise<VM> {
    const body: Record<string, unknown> = { name };
    if (options) {
      if (options.cpus !== undefined) body.cpus = options.cpus;
      if (options.memoryMb !== undefined) body.memory_mb = options.memoryMb;
      if (options.image !== undefined) body.image = options.image;
      if (options.netPolicy !== undefined) body.net_policy = options.netPolicy;
      if (options.ports !== undefined) {
        body.ports = options.ports.map((p: PortMap) => ({
          host_port: p.hostPort,
          guest_port: p.guestPort,
          proto: p.proto,
        }));
      }
    }

    const resp = await this.request<{
      name: string;
      status: string;
      guest_ip?: string;
      pid?: number;
      created_at?: string;
    }>('POST', '/vms', body);

    const info: VMInfo = {
      name: resp.name,
      status: resp.status,
      guestIp: resp.guest_ip,
      pid: resp.pid,
      createdAt: resp.created_at,
    };

    return new VM(this, resp.name, info);
  }

  /** List all known VMs. */
  async list(): Promise<VMInfo[]> {
    const resp = await this.request<
      Array<{
        name: string;
        status: string;
        guest_ip?: string;
        pid?: number;
        created_at?: string;
      }>
    >('GET', '/vms');

    return resp.map((v) => ({
      name: v.name,
      status: v.status,
      guestIp: v.guest_ip,
      pid: v.pid,
      createdAt: v.created_at,
    }));
  }

  /** Build a custom rootfs image from a list of build steps. */
  async build(steps: BuildStep[], tag: string, sizeMb?: number): Promise<void> {
    await this.request('POST', '/build', {
      image_name: tag,
      steps,
      size_mb: sizeMb,
    });
  }

  /** List all available custom rootfs images. */
  async images(): Promise<ImageInfo[]> {
    const resp = await this.request<Array<{ name: string; size_mb: number }>>('GET', '/images');
    return resp.map((i) => ({ name: i.name, sizeMb: i.size_mb }));
  }

  /** Delete a custom rootfs image by name. */
  async deleteImage(name: string): Promise<void> {
    await this.request('DELETE', `/images/${name}`);
  }

  /** List all available snapshots. */
  async snapshots(): Promise<SnapshotInfo[]> {
    return this.request<SnapshotInfo[]>('GET', '/snapshots');
  }

  /** Delete a snapshot by name. */
  async deleteSnapshot(name: string): Promise<void> {
    await this.request('DELETE', `/snapshots/${name}`);
  }

  /** Get warm pool status. */
  async poolStatus(): Promise<PoolStatus> {
    return this.request<PoolStatus>('GET', '/pool');
  }

  /** Trigger pool warming. Returns immediately; warming is async. */
  async poolWarm(): Promise<void> {
    await this.request('POST', '/pool/warm');
  }
}
