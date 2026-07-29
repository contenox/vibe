//go:build llamacpp_direct

// JSON-schema → GBNF bridge over llama.cpp's common library, so the llama
// backend can serve structured output with the same schema payloads the
// runtime already produces for other backends.

#include "json-schema-to-grammar.h"

#include <nlohmann/json.hpp>

#include <cstdlib>
#include <cstring>
#include <exception>
#include <string>

using json = nlohmann::ordered_json;

static char *cx_grammar_strdup(const std::string & value) {
    char *out = static_cast<char *>(std::malloc(value.size() + 1));
    if (out == nullptr) {
        return nullptr;
    }
    std::memcpy(out, value.data(), value.size());
    out[value.size()] = '\0';
    return out;
}

// cx_json_schema_to_grammar converts a JSON schema document into a GBNF
// grammar rooted at "root". On success returns 0 and sets *out (caller frees).
// On failure returns 1 and sets *out to the error text (caller frees).
extern "C" int cx_json_schema_to_grammar(const char *schema_json, char **out) {
    if (out == nullptr) {
        return 1;
    }
    *out = nullptr;
    if (schema_json == nullptr || schema_json[0] == '\0') {
        *out = cx_grammar_strdup("empty JSON schema");
        return 1;
    }
    try {
        const json schema = json::parse(schema_json);
        const std::string grammar = json_schema_to_grammar(schema);
        if (grammar.empty()) {
            *out = cx_grammar_strdup("schema produced an empty grammar");
            return 1;
        }
        *out = cx_grammar_strdup(grammar);
        return *out == nullptr ? 1 : 0;
    } catch (const std::exception &e) {
        *out = cx_grammar_strdup(std::string("convert JSON schema to grammar: ") + e.what());
        return 1;
    }
}
