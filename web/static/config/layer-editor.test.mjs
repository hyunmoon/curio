import test from 'node:test';
import assert from 'node:assert/strict';
import { validateLayerShape, assertLayerLoaded } from './layer-editor.mjs';

const schema = {
    $ref: '#/$defs/Root',
    $defs: {
        Root: { type: 'object', properties: { Subsystems: { $ref: '#/definitions/Subsystems' }, Ingest: { $ref: '#/$defs/Ingest' } } },
        Ingest: { type: 'object', properties: { MK20PipelineInsertBatch: { type: 'integer' }, MK20PipelineInsertMaxActive: { type: 'integer' } } },
    },
    definitions: { Subsystems: { type: 'object', properties: {
        SealSDRMinStartInterval: { type: 'string' }, SealSDRStartJitter: { type: 'boolean' }, SealSDRMaxTasks: { type: 'integer' },
    } } },
};

test('multiple custom overrides, no-edit and one-field edit preserve others', () => {
    for (const [tasks, interval] of [[4, '43m45s'], [6, '25m20s']]) {
        const original = { Subsystems: { SealSDRMaxTasks: tasks, SealSDRMinStartInterval: interval, SealSDRStartJitter: true },
            Ingest: { MK20PipelineInsertBatch: 0, MK20PipelineInsertMaxActive: 17 } };
        const loaded = structuredClone(original);
        validateLayerShape(loaded, schema);
        assertLayerLoaded(original, loaded);
        loaded.Subsystems.SealSDRMaxTasks++;
        validateLayerShape(loaded, schema);
        assert.equal(loaded.Subsystems.SealSDRMinStartInterval, interval);
        assert.deepEqual(loaded.Ingest, original.Ingest);
    }
});

test('missing, coerced and unknown fields stop destructive save', () => {
    const original = { Subsystems: { SealSDRStartJitter: false } };
    assert.throws(() => assertLayerLoaded(original, { Subsystems: {} }), /Editor lost/);
    assert.throws(() => assertLayerLoaded(original, { Subsystems: { SealSDRStartJitter: true } }), /Editor changed/);
    assert.throws(() => validateLayerShape({ Subsystems: { UnrecognizedOption: 7 } }, schema), /Unsupported configuration field/);
    assert.throws(() => validateLayerShape({ Subsystems: { SealSDRStartJitter: {} } }, schema), /type mismatch/);
});

test('invalid references, NULL and unsafe integer precision fail closed', () => {
    assert.throws(() => validateLayerShape({}, { $ref: '#/$defs/Missing' }), /Unresolved/);
    assert.throws(() => validateLayerShape({ Subsystems: null }, schema), /NULL/);
    assert.throws(() => validateLayerShape({ Ingest: { MK20PipelineInsertBatch: 2 ** 53 } }, schema), /safely representable/);
});

test('arbitrary nested maps/arrays and empty strings survive without cleanup', () => {
    const s = { type: 'object', additionalProperties: { type: 'array', items: { type: 'string' } } };
    const layer = { ArbitraryKey: ['', 'value'], Empty: [] };
    validateLayerShape(layer, s);
    assertLayerLoaded(layer, structuredClone(layer));
    assert.deepEqual(layer.ArbitraryKey, ['', 'value']);
});
