/** Options for creating a new VM. */
export interface CreateVMOptions {
  cpus?: number;
  memoryMb?: number;
  image?: string;
  netPolicy?: string;
  ports?: PortMap[];
}

/** A host-to-guest port mapping. */
export interface PortMap {
  hostPort: number;
  guestPort: number;
  proto?: string;
}

/** API representation of a virtual machine. */
export interface VMInfo {
  name: string;
  status: string;
  guestIp?: string;
  pid?: number;
  createdAt?: string;
}

/** Result of executing a command inside a VM. */
export interface ExecResult {
  output: string;
  exitCode: number;
}

/** Describes a VM snapshot. */
export interface SnapshotInfo {
  name: string;
  vm?: string;
  created?: string;
  type?: string;
}

/** Describes a custom rootfs image. */
export interface ImageInfo {
  name: string;
  sizeMb: number;
}

/** Warm pool status counts. */
export interface PoolStatus {
  ready: number;
  total: number;
}

/** A single step in a build recipe. */
export interface BuildStep {
  directive: string;
  args: string;
}

/** Options for creating a Sandbox client. */
export interface SandboxOptions {
  remote: string;
  apiKey?: string;
}
