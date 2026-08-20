//! A loopback OpenAPI service for the suite to register with
//! `contenox tools add`. It serves a one-operation spec, records every request
//! it is sent as a JSON line, and can demand a login before it answers.
//!
//! It listens on 127.0.0.1 only and talks to nothing, so a case that uses it
//! still needs no network and no credentials.
//!
//! Usage: openapi_stub <journal-path> [--require-auth]
//! Prints `PORT=<n>` on stdout once it is listening.

use std::collections::BTreeMap;
use std::fs::OpenOptions;
use std::io::{BufRead, BufReader, Read, Write};
use std::net::TcpListener;

const SPEC: &str = r#"{
  "openapi": "3.0.0",
  "info": {"title": "ledger", "version": "1.0.0"},
  "paths": {
    "/entries": {
      "get": {
        "operationId": "list_entries",
        "summary": "List ledger entries",
        "parameters": [
          {"name": "since", "in": "query", "required": false, "schema": {"type": "string"}},
          {"name": "region", "in": "query", "required": false, "schema": {"type": "string"}}
        ],
        "responses": {"200": {"description": "ok"}}
      }
    }
  }
}"#;

fn main() {
    let mut args = std::env::args().skip(1);
    let journal = args.next().expect("a journal path");
    let require_auth = args.any(|arg| arg == "--require-auth");

    let listener = TcpListener::bind("127.0.0.1:0").expect("bind loopback");
    println!("PORT={}", listener.local_addr().expect("addr").port());
    std::io::stdout().flush().expect("flush the port");

    for stream in listener.incoming() {
        let Ok(mut stream) = stream else { continue };
        let mut reader = BufReader::new(stream.try_clone().expect("clone the socket"));

        let mut request_line = String::new();
        if reader.read_line(&mut request_line).is_err() || request_line.trim().is_empty() {
            continue;
        }
        let mut parts = request_line.split_whitespace();
        let method = parts.next().unwrap_or_default().to_string();
        let target = parts.next().unwrap_or_default().to_string();

        let mut headers: BTreeMap<String, String> = BTreeMap::new();
        loop {
            let mut line = String::new();
            if reader.read_line(&mut line).unwrap_or(0) == 0 {
                break;
            }
            if line.trim().is_empty() {
                break;
            }
            if let Some((name, value)) = line.split_once(':') {
                headers.insert(name.trim().to_lowercase(), value.trim().to_string());
            }
        }

        let length: usize = headers
            .get("content-length")
            .and_then(|value| value.parse().ok())
            .unwrap_or(0);
        let mut body = vec![0u8; length];
        if length > 0 {
            let _ = reader.read_exact(&mut body);
        }
        let body = String::from_utf8_lossy(&body).into_owned();

        let (path, query) = match target.split_once('?') {
            Some((path, query)) => (path.to_string(), query.to_string()),
            None => (target.clone(), String::new()),
        };

        let record = serde_json::json!({
            "method": method,
            "path": path,
            "query": query,
            "headers": headers,
            "body": body,
        });
        let mut file = OpenOptions::new()
            .create(true)
            .append(true)
            .open(&journal)
            .expect("open the journal");
        writeln!(file, "{record}").expect("record the request");

        let (status, payload) = match path.as_str() {
            "/openapi.json" => ("200 OK", SPEC.to_string()),
            "/login" => (
                "200 OK",
                r#"{"data":{"token":"issued-by-the-login-flow"}}"#.to_string(),
            ),
            "/entries" if require_auth && !headers.contains_key("authorization") => (
                "401 Unauthorized",
                r#"{"error":"log in first"}"#.to_string(),
            ),
            "/entries" => (
                "200 OK",
                r#"{"entries":["the ledger answered"]}"#.to_string(),
            ),
            _ => ("404 Not Found", r#"{"error":"no such path"}"#.to_string()),
        };

        let response = format!(
            "HTTP/1.1 {status}\r\nContent-Type: application/json\r\nContent-Length: {}\r\nConnection: close\r\n\r\n{payload}",
            payload.len()
        );
        let _ = stream.write_all(response.as_bytes());
        let _ = stream.flush();
    }
}
