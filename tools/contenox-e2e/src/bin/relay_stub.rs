//! A loopback stand-in for the relay, for the cases about pairing.
//!
//! It answers `POST /v1/pair/redeem` — the one call `contenox pair` and
//! `/pair` make — records every request it is sent as a JSON line, and notes
//! the TLS connections a paired machine opens when it dials out. It listens on
//! 127.0.0.1 only and talks to nothing, so a case that uses it still needs no
//! network and no credentials.
//!
//! Usage: relay_stub <journal-path> [flags]
//!   --accept <key>      only these keys are known (repeatable; default: any)
//!   --instance <id>     the instance id handed back      (default: instance-1)
//!   --account <id>      the account id handed back       (default: acct-e2e)
//!   --token <secret>    the instance token handed back
//!   --no-public-key     answer without a relay public key
//!
//! Prints `PORT=<n>` on stdout once it is listening.

use std::collections::{BTreeMap, BTreeSet};
use std::fs::OpenOptions;
use std::io::{BufRead, BufReader, Read, Write};
use std::net::TcpListener;

/// The identity key this build pins for the hosted relay. A stand-in hands out
/// the same one so a redeemed pairing is one the binary will accept anywhere.
const RELAY_PUBLIC_KEY: &str = "K8uMWEgeCMJ5FYpHKWUmUbCgC1nRjiR8/ORzlUVYzHE=";

struct Options {
    journal: String,
    accept: Vec<String>,
    instance: String,
    account: String,
    token: String,
    public_key: Option<String>,
}

fn options() -> Options {
    let mut args = std::env::args().skip(1);
    let journal = args.next().expect("a journal path");
    let mut opts = Options {
        journal,
        accept: Vec::new(),
        instance: "instance-1".into(),
        account: "acct-e2e".into(),
        token: "instance-token-e2e".into(),
        public_key: Some(RELAY_PUBLIC_KEY.into()),
    };
    while let Some(flag) = args.next() {
        match flag.as_str() {
            "--accept" => opts.accept.push(args.next().expect("--accept <key>")),
            "--instance" => opts.instance = args.next().expect("--instance <id>"),
            "--account" => opts.account = args.next().expect("--account <id>"),
            "--token" => opts.token = args.next().expect("--token <secret>"),
            "--no-public-key" => opts.public_key = None,
            other => panic!("relay_stub: unknown flag {other:?}"),
        }
    }
    opts
}

fn main() {
    let opts = options();
    let listener = TcpListener::bind("127.0.0.1:0").expect("bind loopback");
    println!("PORT={}", listener.local_addr().expect("addr").port());
    std::io::stdout().flush().expect("flush the port");

    let mut redeemed: BTreeSet<String> = BTreeSet::new();

    for stream in listener.incoming() {
        let Ok(mut stream) = stream else { continue };
        let mut reader = BufReader::new(stream.try_clone().expect("clone the socket"));

        // A dial arrives as TLS: the machine speaks first, and its ClientHello
        // starts a handshake record. There is no certificate here to finish it
        // with — that the connection was opened at all is the observation.
        let peeked = reader.fill_buf().map(<[u8]>::to_vec).unwrap_or_default();
        if peeked.first() == Some(&0x16) {
            record(&opts.journal, serde_json::json!({"event": "dial"}));
            continue;
        }

        let mut request_line = String::new();
        if reader.read_line(&mut request_line).is_err() || request_line.trim().is_empty() {
            continue;
        }
        let mut parts = request_line.split_whitespace();
        let method = parts.next().unwrap_or_default().to_string();
        let path = parts.next().unwrap_or_default().to_string();

        let mut headers: BTreeMap<String, String> = BTreeMap::new();
        loop {
            let mut line = String::new();
            if reader.read_line(&mut line).unwrap_or(0) == 0 || line.trim().is_empty() {
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

        record(
            &opts.journal,
            serde_json::json!({
                "event": "request",
                "method": method,
                "path": path,
                "headers": headers,
                "body": body,
            }),
        );

        let (status, payload) = if path == "/v1/pair/redeem" {
            redeem(&opts, &body, &mut redeemed)
        } else {
            (
                "404 Not Found",
                r#"{"error":{"message":"no such path"}}"#.to_string(),
            )
        };

        let response = format!(
            "HTTP/1.1 {status}\r\nContent-Type: application/json\r\nContent-Length: {}\r\nConnection: close\r\n\r\n{payload}",
            payload.len()
        );
        let _ = stream.write_all(response.as_bytes());
        let _ = stream.flush();
    }
}

/// The relay's decision about one key: known, unused, and only then a
/// credential.
fn redeem(opts: &Options, body: &str, redeemed: &mut BTreeSet<String>) -> (&'static str, String) {
    let sent: serde_json::Value = serde_json::from_str(body).unwrap_or(serde_json::Value::Null);
    let key = sent
        .get("key")
        .and_then(serde_json::Value::as_str)
        .unwrap_or_default()
        .to_string();

    if !opts.accept.is_empty() && !opts.accept.contains(&key) {
        return (
            "400 Bad Request",
            r#"{"error":{"message":"no such pairing key: it expired or was never minted"}}"#
                .to_string(),
        );
    }
    if !redeemed.insert(key) {
        return (
            "400 Bad Request",
            r#"{"error":{"message":"this pairing key has already been redeemed"}}"#.to_string(),
        );
    }

    let mut answer = serde_json::json!({
        "instance_token": opts.token,
        "instance_id": opts.instance,
        "account_id": opts.account,
    });
    if let Some(key) = &opts.public_key {
        answer["relay_public_key"] = serde_json::Value::String(key.clone());
    }
    ("200 OK", answer.to_string())
}

fn record(journal: &str, entry: serde_json::Value) {
    let mut file = OpenOptions::new()
        .create(true)
        .append(true)
        .open(journal)
        .expect("open the journal");
    writeln!(file, "{entry}").expect("record the entry");
}
