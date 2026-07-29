//go:build llamacpp_direct

#include "chat.h"
#include "chat-peg-parser.h"
#include "llama.h"

#include <nlohmann/json.hpp>

#include <cstdio>
#include <cstdlib>
#include <cstring>
#include <exception>
#include <string>
#include <stdexcept>

using json = nlohmann::ordered_json;

extern "C" struct cx_chat_apply_result {
    char *prompt;
    char *parser;
    char *generation_prompt;
    int format;
};

extern "C" struct cx_chat_syntax_result {
    char *parser;
    int format;
};

static char *cx_strdup_string(const std::string & value) {
    char *out = static_cast<char *>(std::malloc(value.size() + 1));
    if (out == nullptr) {
        return nullptr;
    }
    std::memcpy(out, value.data(), value.size());
    out[value.size()] = '\0';
    return out;
}

static common_reasoning_format cx_reasoning_format_or_none(const char *name) {
    if (name == nullptr || name[0] == '\0') {
        return COMMON_REASONING_FORMAT_NONE;
    }
    return common_reasoning_format_from_name(std::string(name));
}

extern "C" struct cx_chat_apply_result cx_common_chat_apply(
    const struct llama_model *model,
    const char *messages_json,
    const char *tools_json,
    int add_generation_prompt,
    const char *reasoning_format,
    int enable_thinking,
    const char *reasoning_effort,
    char *errbuf,
    size_t errlen) {
    struct cx_chat_apply_result result = {};
    try {
        if (model == nullptr) {
            throw std::runtime_error("model is null");
        }

        auto tmpls = common_chat_templates_init(model, "");

        common_chat_templates_inputs inputs;
        inputs.use_jinja = true;
        inputs.add_generation_prompt = add_generation_prompt != 0;
        inputs.reasoning_format = cx_reasoning_format_or_none(reasoning_format);
        inputs.enable_thinking = enable_thinking != 0;
        // Model-owned effort levels (e.g. gpt-oss harmony templates read
        // reasoning_effort). Templates without the variable ignore it.
        if (reasoning_effort != nullptr && reasoning_effort[0] != '\0') {
            inputs.chat_template_kwargs["reasoning_effort"] =
                std::string("\"") + reasoning_effort + "\"";
        }
        inputs.messages = common_chat_msgs_parse_oaicompat(
            json::parse(messages_json != nullptr ? messages_json : "[]"));
        if (tools_json != nullptr && tools_json[0] != '\0') {
            inputs.tools = common_chat_tools_parse_oaicompat(json::parse(tools_json));
        }

        auto params = common_chat_templates_apply(tmpls.get(), inputs);
        char *out = cx_strdup_string(params.prompt);
        char *parser = cx_strdup_string(params.parser);
        char *generation_prompt = cx_strdup_string(params.generation_prompt);
        if (out == nullptr || parser == nullptr || generation_prompt == nullptr) {
            std::free(out);
            std::free(parser);
            std::free(generation_prompt);
            throw std::runtime_error("out of memory");
        }
        result.prompt = out;
        result.parser = parser;
        result.generation_prompt = generation_prompt;
        result.format = static_cast<int>(params.format);
        return result;
    } catch (const std::exception &e) {
        if (errbuf != nullptr && errlen > 0) {
            std::snprintf(errbuf, errlen, "%s", e.what());
        }
        return result;
    } catch (...) {
        if (errbuf != nullptr && errlen > 0) {
            std::snprintf(errbuf, errlen, "unknown common chat template error");
        }
        return result;
    }
}

extern "C" int cx_common_chat_parse(
    const char *input,
    int is_partial,
    int format,
    const char *parser_json,
    const char *generation_prompt,
    const char *reasoning_format,
    int parse_tool_calls,
    char **content_out,
    char **reasoning_out,
    char **tool_calls_out,
    char *errbuf,
    size_t errlen) {
    try {
        if (content_out == nullptr || reasoning_out == nullptr || tool_calls_out == nullptr) {
            throw std::runtime_error("output pointers are null");
        }
        *content_out = nullptr;
        *reasoning_out = nullptr;
        *tool_calls_out = nullptr;

        common_chat_parser_params params;
        params.format = static_cast<common_chat_format>(format);
        params.reasoning_format = cx_reasoning_format_or_none(reasoning_format);
        params.reasoning_in_content = false;
        params.parse_tool_calls = parse_tool_calls != 0;
        params.generation_prompt = generation_prompt != nullptr ? std::string(generation_prompt) : std::string();
        if (parser_json != nullptr && parser_json[0] != '\0') {
            params.parser.load(std::string(parser_json));
        }

        auto msg = common_chat_parse(
            std::string(input != nullptr ? input : ""),
            is_partial != 0,
            params);
        json tool_calls = json::array();
        for (const auto & tc : msg.tool_calls) {
            json item {
                {"type", "function"},
                {"function", {
                    {"name", tc.name},
                    {"arguments", tc.arguments},
                }},
            };
            if (!tc.id.empty()) {
                item["id"] = tc.id;
            }
            tool_calls.push_back(item);
        }
        *content_out = cx_strdup_string(msg.content);
        *reasoning_out = cx_strdup_string(msg.reasoning_content);
        *tool_calls_out = cx_strdup_string(tool_calls.dump());
        if (*content_out == nullptr || *reasoning_out == nullptr || *tool_calls_out == nullptr) {
            if (*content_out != nullptr) {
                std::free(*content_out);
                *content_out = nullptr;
            }
            if (*reasoning_out != nullptr) {
                std::free(*reasoning_out);
                *reasoning_out = nullptr;
            }
            if (*tool_calls_out != nullptr) {
                std::free(*tool_calls_out);
                *tool_calls_out = nullptr;
            }
            throw std::runtime_error("out of memory");
        }
        return 0;
    } catch (const std::exception &e) {
        if (errbuf != nullptr && errlen > 0) {
            std::snprintf(errbuf, errlen, "%s", e.what());
        }
        return -1;
    } catch (...) {
        if (errbuf != nullptr && errlen > 0) {
            std::snprintf(errbuf, errlen, "unknown common chat parse error");
        }
        return -1;
    }
}

extern "C" struct cx_chat_probe_result {
    char *format_name;
    char *thinking_start_tag;
    int supports_tool_calls;
    int supports_thinking;
    int supports_reasoning_effort;
};

// cx_probe_chat_template inspects a model's own chat template without loading
// tensors (vocab-only model load): what llama.cpp's common_chat engine detects
// is what the linked runtime can actually render and parse — tool-call syntax,
// a thinking toggle, and model-owned reasoning-effort levels. This is the
// capability-truth source; curated protocol tables are overrides on top of it.
extern "C" struct cx_chat_probe_result cx_probe_chat_template(
    const char *model_path,
    char *errbuf,
    size_t errlen) {
    struct cx_chat_probe_result result = {};
    llama_model *model = nullptr;
    try {
        if (model_path == nullptr || model_path[0] == '\0') {
            throw std::runtime_error("model path is empty");
        }
        llama_model_params mp = llama_model_default_params();
        mp.vocab_only = true;
        model = llama_model_load_from_file(model_path, mp);
        if (model == nullptr) {
            throw std::runtime_error("vocab-only model load failed");
        }
        auto tmpls = common_chat_templates_init(model, "");

        common_chat_templates_inputs base;
        base.use_jinja = true;
        base.add_generation_prompt = true;
        base.messages = common_chat_msgs_parse_oaicompat(json::parse(
            R"([{"role":"user","content":"ping"}])"));
        auto plain_params = common_chat_templates_apply(tmpls.get(), base);

        // Tool support: render with one dummy tool; a template that engages a
        // tool-call syntax changes the prompt and yields parser metadata.
        auto with_tools = base;
        with_tools.tools = common_chat_tools_parse_oaicompat(json::parse(
            R"([{"type":"function","function":{"name":"probe","description":"probe","parameters":{"type":"object","properties":{}}}}])"));
        auto tool_params = common_chat_templates_apply(tmpls.get(), with_tools);
        result.supports_tool_calls =
            (tool_params.prompt != plain_params.prompt &&
             (tool_params.format != COMMON_CHAT_FORMAT_CONTENT_ONLY || !tool_params.parser.empty())) ? 1 : 0;

        // Thinking toggle + tag come straight from the template engine.
        result.supports_thinking = common_chat_templates_support_enable_thinking(tmpls.get()) ? 1 : 0;
        result.thinking_start_tag = cx_strdup_string(plain_params.thinking_start_tag);

        // Effort levels: real iff different levels render different prompts.
        auto low = base;
        low.chat_template_kwargs["reasoning_effort"] = "\"low\"";
        auto high = base;
        high.chat_template_kwargs["reasoning_effort"] = "\"high\"";
        try {
            auto low_params = common_chat_templates_apply(tmpls.get(), low);
            auto high_params = common_chat_templates_apply(tmpls.get(), high);
            result.supports_reasoning_effort = (low_params.prompt != high_params.prompt) ? 1 : 0;
        } catch (...) {
            result.supports_reasoning_effort = 0;
        }

        result.format_name = cx_strdup_string(common_chat_format_name(tool_params.format));
        if (result.format_name == nullptr || result.thinking_start_tag == nullptr) {
            std::free(result.format_name);
            std::free(result.thinking_start_tag);
            result.format_name = nullptr;
            result.thinking_start_tag = nullptr;
            throw std::runtime_error("out of memory");
        }
        llama_model_free(model);
        return result;
    } catch (const std::exception &e) {
        if (model != nullptr) {
            llama_model_free(model);
        }
        if (errbuf != nullptr && errlen > 0) {
            std::snprintf(errbuf, errlen, "%s", e.what());
        }
        return result;
    } catch (...) {
        if (model != nullptr) {
            llama_model_free(model);
        }
        if (errbuf != nullptr && errlen > 0) {
            std::snprintf(errbuf, errlen, "unknown chat template probe error");
        }
        return result;
    }
}

extern "C" struct cx_chat_syntax_result cx_common_chat_syntax_openai_tools(
    const char *tools_json,
    char *errbuf,
    size_t errlen) {
    struct cx_chat_syntax_result result = {};
    try {
        auto tools = json::parse(tools_json != nullptr ? tools_json : "[]");
        auto parser = build_chat_peg_parser([&](common_chat_peg_builder & p) {
            return p.standard_json_tools(
                "{\"tool_calls\":",
                "}",
                tools,
                true,
                true,
                "function.name",
                "function.arguments",
                true,
                false,
                "id") + p.end();
        });
        result.parser = cx_strdup_string(parser.save());
        if (result.parser == nullptr) {
            throw std::runtime_error("out of memory");
        }
        result.format = static_cast<int>(COMMON_CHAT_FORMAT_PEG_NATIVE);
        return result;
    } catch (const std::exception &e) {
        if (errbuf != nullptr && errlen > 0) {
            std::snprintf(errbuf, errlen, "%s", e.what());
        }
        return result;
    } catch (...) {
        if (errbuf != nullptr && errlen > 0) {
            std::snprintf(errbuf, errlen, "unknown common chat syntax error");
        }
        return result;
    }
}
