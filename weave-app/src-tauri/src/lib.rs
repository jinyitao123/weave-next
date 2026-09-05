use chrono::{SecondsFormat, Utc};
use serde::{Deserialize, Serialize};
use std::{
    fs,
    path::PathBuf,
    process::{Child, Command, Stdio},
    sync::Mutex,
};
use tauri::{AppHandle, Manager, RunEvent, State};

const KEYCHAIN_SERVICE: &str = "com.weave.desktop";
const KEYCHAIN_ACCOUNT: &str = "session-token";
const RUNTIME_WORKSPACES_DIR: &str = "runtime-workspaces";

#[derive(Default)]
struct RuntimeProcess {
    child: Option<Child>,
    started_at: Option<String>,
    last_error: Option<String>,
}

struct RuntimeState(Mutex<RuntimeProcess>);

#[derive(Serialize)]
#[serde(rename_all = "camelCase")]
struct LocalRuntimeStatus {
    running: bool,
    #[serde(skip_serializing_if = "Option::is_none")]
    pid: Option<u32>,
    #[serde(skip_serializing_if = "Option::is_none")]
    started_at: Option<String>,
    #[serde(skip_serializing_if = "Option::is_none")]
    last_error: Option<String>,
}

#[derive(Deserialize)]
#[serde(rename_all = "camelCase")]
struct StartLocalRuntimeRequest {
    server_url: String,
    runtime_token: String,
    concurrency: u32,
    binary_path: Option<String>,
}

fn app_data_dir(app: &AppHandle) -> Result<PathBuf, String> {
    app.path()
        .app_data_dir()
        .map_err(|error| format!("resolve app data directory: {error}"))
}

fn keychain_entry() -> Result<keyring::Entry, String> {
    keyring::Entry::new(KEYCHAIN_SERVICE, KEYCHAIN_ACCOUNT)
        .map_err(|error| format!("open credential store: {error}"))
}

#[tauri::command]
fn read_auth_token() -> Result<Option<String>, String> {
    match keychain_entry()?.get_password() {
        Ok(token) => Ok(Some(token)),
        Err(keyring::Error::NoEntry) => Ok(None),
        Err(error) => Err(format!("read credential: {error}")),
    }
}

#[tauri::command]
fn write_auth_token(token: String) -> Result<(), String> {
    if token.is_empty() {
        return Err("credential must not be empty".into());
    }
    keychain_entry()?
        .set_password(&token)
        .map_err(|error| format!("write credential: {error}"))
}

#[tauri::command]
fn clear_auth_token() -> Result<(), String> {
    match keychain_entry()?.delete_credential() {
        Ok(()) | Err(keyring::Error::NoEntry) => Ok(()),
        Err(error) => Err(format!("clear credential: {error}")),
    }
}

fn reap_runtime(process: &mut RuntimeProcess) {
    let Some(child) = process.child.as_mut() else {
        return;
    };
    match child.try_wait() {
        Ok(None) => {}
        Ok(Some(status)) => {
            process.child = None;
            process.started_at = None;
            process.last_error = Some(format!("Local runtime exited with status {status}"));
        }
        Err(error) => {
            process.last_error = Some(format!("inspect local runtime: {error}"));
        }
    }
}

fn runtime_status(process: &RuntimeProcess) -> LocalRuntimeStatus {
    LocalRuntimeStatus {
        running: process.child.is_some(),
        pid: process.child.as_ref().map(Child::id),
        started_at: process.started_at.clone(),
        last_error: process.last_error.clone(),
    }
}

#[tauri::command]
fn inspect_local_runtime(state: State<'_, RuntimeState>) -> Result<LocalRuntimeStatus, String> {
    let mut process = state
        .0
        .lock()
        .map_err(|_| "local runtime state is unavailable".to_string())?;
    reap_runtime(&mut process);
    Ok(runtime_status(&process))
}

#[tauri::command]
fn start_local_runtime(
    app: AppHandle,
    request: StartLocalRuntimeRequest,
    state: State<'_, RuntimeState>,
) -> Result<LocalRuntimeStatus, String> {
    let server_url = url::Url::parse(request.server_url.trim())
        .map_err(|_| "server URL must be a valid HTTP or HTTPS URL".to_string())?;
    if !matches!(server_url.scheme(), "http" | "https") || server_url.host_str().is_none() {
        return Err("server URL must be a valid HTTP or HTTPS URL".into());
    }
    if request.runtime_token.is_empty() {
        return Err("runtime token is required".into());
    }
    if request.concurrency < 1 {
        return Err("concurrency must be at least 1".into());
    }

    let mut process = state
        .0
        .lock()
        .map_err(|_| "local runtime state is unavailable".to_string())?;
    reap_runtime(&mut process);
    if process.child.is_some() {
        return Err("local runtime is already running".into());
    }

    let workspaces_root = app_data_dir(&app)?.join(RUNTIME_WORKSPACES_DIR);
    fs::create_dir_all(&workspaces_root)
        .map_err(|error| format!("create runtime workspaces directory: {error}"))?;
    let binary = request
        .binary_path
        .as_deref()
        .filter(|path| !path.trim().is_empty())
        .unwrap_or("weave");
    let spawn_result = Command::new(binary)
        .arg("runtime")
        .arg("--server")
        .arg(server_url.as_str().trim_end_matches('/'))
        .arg("--workspaces-root")
        .arg(&workspaces_root)
        .arg("--concurrency")
        .arg(request.concurrency.to_string())
        .env("WEAVE_RUNTIME_TOKEN", request.runtime_token)
        .stdin(Stdio::null())
        .stdout(Stdio::null())
        .stderr(Stdio::null())
        .spawn();

    match spawn_result {
        Ok(child) => {
            process.child = Some(child);
            process.started_at = Some(Utc::now().to_rfc3339_opts(SecondsFormat::Secs, true));
            process.last_error = None;
            Ok(runtime_status(&process))
        }
        Err(error) => {
            let message = format!("start local runtime: {error}");
            process.last_error = Some(message.clone());
            Err(message)
        }
    }
}

fn stop_runtime(process: &mut RuntimeProcess) -> Result<(), String> {
    reap_runtime(process);
    let Some(mut child) = process.child.take() else {
        process.started_at = None;
        return Ok(());
    };
    if let Err(error) = child.kill() {
        process.child = Some(child);
        let message = format!("stop local runtime: {error}");
        process.last_error = Some(message.clone());
        return Err(message);
    }
    if let Err(error) = child.wait() {
        let message = format!("reap local runtime: {error}");
        process.child = Some(child);
        process.last_error = Some(message.clone());
        return Err(message);
    }
    process.started_at = None;
    process.last_error = None;
    Ok(())
}

#[tauri::command]
fn stop_local_runtime(state: State<'_, RuntimeState>) -> Result<LocalRuntimeStatus, String> {
    let mut process = state
        .0
        .lock()
        .map_err(|_| "local runtime state is unavailable".to_string())?;
    stop_runtime(&mut process)?;
    Ok(runtime_status(&process))
}

pub fn run() {
    let app = tauri::Builder::default()
        .manage(RuntimeState(Mutex::new(RuntimeProcess::default())))
        .invoke_handler(tauri::generate_handler![
            read_auth_token,
            write_auth_token,
            clear_auth_token,
            start_local_runtime,
            inspect_local_runtime,
            stop_local_runtime,
        ])
        .build(tauri::generate_context!())
        .expect("failed to build Weave desktop application");

    app.run(|app, event| {
        if matches!(event, RunEvent::Exit | RunEvent::ExitRequested { .. }) {
            if let Ok(mut process) = app.state::<RuntimeState>().0.lock() {
                let _ = stop_runtime(&mut process);
            }
        }
    });
}
