export interface PluginInstallation {
  id: string;
  plugin_key: string;
  display_name: string;
  desired_version: string;
  active_version?: string;
  enabled: boolean;
  desired_generation: number;
  active_generation: number;
  lifecycle_status: string;
  health_state?: string;
  health_reason?: string;
  contributions: string[];
}

export interface PluginCatalogEntry {
  plugin_key: string;
  version: string;
  bundled: boolean;
  contributions: string[];
}

export interface ListWorkspacePluginsResponse {
  plugins: PluginInstallation[];
  catalog: PluginCatalogEntry[];
}
