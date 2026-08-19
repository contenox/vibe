use serde::Serialize;
use serde_json::{Map, Value};

#[derive(Debug, Clone, Default, Serialize)]
pub struct Script {
    #[serde(skip_serializing_if = "Option::is_none")]
    pub model: Option<String>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub context_length: Option<u32>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub max_output_tokens: Option<u32>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub embed_dimensions: Option<u32>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub capabilities: Option<Capabilities>,
    pub turns: Vec<Turn>,
}

impl Script {
    pub fn new() -> Self {
        Self::default()
    }

    pub fn model(mut self, model: impl Into<String>) -> Self {
        self.model = Some(model.into());
        self
    }

    pub fn context_length(mut self, tokens: u32) -> Self {
        self.context_length = Some(tokens);
        self
    }

    pub fn max_output_tokens(mut self, tokens: u32) -> Self {
        self.max_output_tokens = Some(tokens);
        self
    }

    pub fn embed_dimensions(mut self, width: u32) -> Self {
        self.embed_dimensions = Some(width);
        self
    }

    pub fn capabilities(mut self, capabilities: Capabilities) -> Self {
        self.capabilities = Some(capabilities);
        self
    }

    pub fn turn(mut self, turn: impl Into<Turn>) -> Self {
        self.turns.push(turn.into());
        self
    }

    pub fn turns<I, T>(mut self, turns: I) -> Self
    where
        I: IntoIterator<Item = T>,
        T: Into<Turn>,
    {
        self.turns.extend(turns.into_iter().map(Into::into));
        self
    }

    pub fn route(self, label: impl Into<String>) -> Self {
        self.turn(Turn::new().text(label))
    }

    pub fn to_json(&self) -> String {
        serde_json::to_string_pretty(self).expect("a Script always serialises")
    }
}

#[derive(Debug, Clone, Default, Serialize)]
pub struct Turn {
    #[serde(skip_serializing_if = "Option::is_none")]
    pub text: Option<String>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub thinking: Option<String>,
    #[serde(skip_serializing_if = "Vec::is_empty")]
    pub tool_calls: Vec<ToolCall>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub finish_reason: Option<String>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub usage: Option<Usage>,
}

impl Turn {
    pub fn new() -> Self {
        Self::default()
    }

    pub fn text(mut self, text: impl Into<String>) -> Self {
        self.text = Some(text.into());
        self
    }

    pub fn thinking(mut self, thinking: impl Into<String>) -> Self {
        self.thinking = Some(thinking.into());
        self
    }

    pub fn call(mut self, call: ToolCall) -> Self {
        self.tool_calls.push(call);
        self
    }

    pub fn finish_reason(mut self, reason: impl Into<String>) -> Self {
        self.finish_reason = Some(reason.into());
        self
    }

    pub fn usage(mut self, usage: Usage) -> Self {
        self.usage = Some(usage);
        self
    }
}

impl From<&str> for Turn {
    fn from(text: &str) -> Self {
        Turn::new().text(text)
    }
}

impl From<String> for Turn {
    fn from(text: String) -> Self {
        Turn::new().text(text)
    }
}

impl From<ToolCall> for Turn {
    fn from(call: ToolCall) -> Self {
        Turn::new().call(call)
    }
}

#[derive(Debug, Clone, Serialize)]
pub struct ToolCall {
    #[serde(skip_serializing_if = "Option::is_none")]
    pub id: Option<String>,
    pub name: String,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub arguments: Option<Value>,
}

impl ToolCall {
    pub fn new(name: impl Into<String>) -> Self {
        Self {
            id: None,
            name: name.into(),
            arguments: None,
        }
    }

    pub fn id(mut self, id: impl Into<String>) -> Self {
        self.id = Some(id.into());
        self
    }

    pub fn arg(mut self, key: impl Into<String>, value: impl Into<Value>) -> Self {
        let mut object = match self.arguments.take() {
            Some(Value::Object(map)) => map,
            _ => Map::new(),
        };
        object.insert(key.into(), value.into());
        self.arguments = Some(Value::Object(object));
        self
    }

    pub fn arguments(mut self, value: Value) -> Self {
        self.arguments = Some(value);
        self
    }

    pub fn raw_arguments(mut self, text: impl Into<String>) -> Self {
        self.arguments = Some(Value::String(text.into()));
        self
    }

    pub fn mission_report(kind: impl Into<String>, summary: impl Into<String>) -> Self {
        ToolCall::new("mission_report")
            .arg("kind", kind.into())
            .arg("summary", summary.into())
    }

    pub fn mission_finish(status: impl Into<String>) -> Self {
        ToolCall::new("mission_finish").arg("status", status.into())
    }
}

#[derive(Debug, Clone, Default, Serialize)]
pub struct Capabilities {
    #[serde(skip_serializing_if = "Option::is_none")]
    pub chat: Option<bool>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub stream: Option<bool>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub prompt: Option<bool>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub embed: Option<bool>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub think: Option<bool>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub vision: Option<bool>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub audio: Option<bool>,
}

impl Capabilities {
    pub fn new() -> Self {
        Self::default()
    }

    pub fn chat(mut self, allowed: bool) -> Self {
        self.chat = Some(allowed);
        self
    }

    pub fn stream(mut self, allowed: bool) -> Self {
        self.stream = Some(allowed);
        self
    }

    pub fn prompt(mut self, allowed: bool) -> Self {
        self.prompt = Some(allowed);
        self
    }

    pub fn embed(mut self, allowed: bool) -> Self {
        self.embed = Some(allowed);
        self
    }

    pub fn think(mut self, allowed: bool) -> Self {
        self.think = Some(allowed);
        self
    }

    pub fn vision(mut self, allowed: bool) -> Self {
        self.vision = Some(allowed);
        self
    }

    pub fn audio(mut self, allowed: bool) -> Self {
        self.audio = Some(allowed);
        self
    }
}

#[derive(Debug, Clone, Copy, Default, Serialize)]
pub struct Usage {
    #[serde(skip_serializing_if = "is_zero")]
    pub prompt_tokens: u32,
    #[serde(skip_serializing_if = "is_zero")]
    pub completion_tokens: u32,
    #[serde(skip_serializing_if = "is_zero")]
    pub total_tokens: u32,
}

fn is_zero(value: &u32) -> bool {
    *value == 0
}
