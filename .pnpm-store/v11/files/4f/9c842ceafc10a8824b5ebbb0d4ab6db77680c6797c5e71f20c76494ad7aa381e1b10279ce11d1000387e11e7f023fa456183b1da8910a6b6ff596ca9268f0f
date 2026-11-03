#!/usr/bin/env node
import { createReadStream, createWriteStream } from 'fs';
import { parseOptions, parseCommitsOptions, parseTagsOptions } from './options.js';
import { loadDataFile, isFileExists } from './utils.js';
export const flags = {
    infile: {
        shortFlag: 'i',
        default: 'CHANGELOG.md',
        type: 'string'
    },
    outfile: {
        shortFlag: 'o',
        type: 'string'
    },
    stdout: {
        type: 'boolean'
    },
    preset: {
        shortFlag: 'p',
        type: 'string'
    },
    pkg: {
        shortFlag: 'k',
        type: 'string'
    },
    append: {
        shortFlag: 'a',
        type: 'boolean'
    },
    releaseCount: {
        shortFlag: 'r',
        type: 'number'
    },
    skipUnstable: {
        type: 'boolean'
    },
    outputUnreleased: {
        shortFlag: 'u',
        type: 'boolean'
    },
    verbose: {
        shortFlag: 'v',
        type: 'boolean'
    },
    config: {
        shortFlag: 'n',
        type: 'string'
    },
    context: {
        shortFlag: 'c',
        type: 'string'
    },
    firstRelease: {
        shortFlag: 'f',
        type: 'boolean'
    },
    lernaPackage: {
        shortFlag: 'l',
        type: 'string'
    },
    tagPrefix: {
        shortFlag: 't',
        type: 'string'
    }
};
export async function runProgram(generator, flags) {
    let { infile, outfile, stdout, verbose, pkg, preset, config, context } = flags;
    generator.readPackage(pkg);
    if (preset) {
        generator.loadPreset(preset);
    }
    if (config) {
        const configOptions = await loadDataFile(config);
        if (configOptions.tags) {
            generator.tags(configOptions.tags);
        }
        if (configOptions.commits || configOptions.parser) {
            generator.commits(configOptions.commits || {}, configOptions.parser);
        }
        if (configOptions.writer) {
            generator.writer(configOptions.writer);
        }
    }
    if (context) {
        const writerContext = await loadDataFile(context);
        if (writerContext) {
            generator.context(writerContext);
        }
    }
    const options = parseOptions(flags);
    if (options) {
        generator.options(options);
    }
    const tagsOptions = parseTagsOptions(flags);
    if (tagsOptions) {
        generator.tags(tagsOptions);
    }
    const commitsOptions = parseCommitsOptions(flags);
    if (commitsOptions) {
        generator.commits(commitsOptions);
    }
    if (verbose) {
        generator.options({
            debug(namespace, payload) {
                console.info(`[${namespace}]:`, payload);
            },
            warn(namespace, payload) {
                console.warn(`[${namespace}]:`, payload);
            }
        });
    }
    outfile ||= infile;
    const sameFile = !stdout && infile === outfile;
    const changelog = generator.write();
    const streams = [];
    if (infile && options?.releaseCount !== 0) {
        const isInfileExists = await isFileExists(infile);
        const input = isInfileExists
            ? createReadStream(infile)
            : [];
        if (!isInfileExists && verbose) {
            console.warn(`"${infile}" does not exist.`);
        }
        if (sameFile) {
            if (options?.append) {
                streams.push(changelog);
            }
            else {
                const buffer = [];
                for await (const chunk of changelog) {
                    buffer.push(chunk);
                }
                let newline = true;
                for await (const chunk of input) {
                    if (newline) {
                        newline = false;
                        if (!String(chunk).startsWith('\n')) {
                            buffer.push('\n');
                        }
                    }
                    buffer.push(chunk);
                }
                streams.push(buffer);
            }
        }
        else if (options?.append) {
            streams.push(input, changelog);
        }
        else {
            streams.push(changelog, input);
        }
    }
    else {
        streams.push(changelog);
    }
    const output = (outfile && !stdout
        ? createWriteStream(outfile, {
            flags: sameFile && options?.append ? 'a' : 'w'
        })
        : process.stdout);
    for (let i = 0, stream, newline; i < streams.length; i++) {
        stream = streams[i];
        newline = i > 0;
        for await (const chunk of stream) {
            if (newline) {
                newline = false;
                if (!String(chunk).startsWith('\n')) {
                    output.write('\n');
                }
            }
            output.write(chunk);
        }
    }
}
//# sourceMappingURL=data:application/json;base64,eyJ2ZXJzaW9uIjozLCJmaWxlIjoiY2xpLmpzIiwic291cmNlUm9vdCI6IiIsInNvdXJjZXMiOlsiLi4vLi4vc3JjL2NsaS9jbGkudHMiXSwibmFtZXMiOltdLCJtYXBwaW5ncyI6IjtBQUNBLE9BQU8sRUFFTCxnQkFBZ0IsRUFDaEIsaUJBQWlCLEVBQ2xCLE1BQU0sSUFBSSxDQUFBO0FBS1gsT0FBTyxFQUNMLFlBQVksRUFDWixtQkFBbUIsRUFDbkIsZ0JBQWdCLEVBQ2pCLE1BQU0sY0FBYyxDQUFBO0FBQ3JCLE9BQU8sRUFDTCxZQUFZLEVBQ1osWUFBWSxFQUNiLE1BQU0sWUFBWSxDQUFBO0FBb0JuQixNQUFNLENBQUMsTUFBTSxLQUFLLEdBQUc7SUFDbkIsTUFBTSxFQUFFO1FBQ04sU0FBUyxFQUFFLEdBQUc7UUFDZCxPQUFPLEVBQUUsY0FBYztRQUN2QixJQUFJLEVBQUUsUUFBUTtLQUNmO0lBQ0QsT0FBTyxFQUFFO1FBQ1AsU0FBUyxFQUFFLEdBQUc7UUFDZCxJQUFJLEVBQUUsUUFBUTtLQUNmO0lBQ0QsTUFBTSxFQUFFO1FBQ04sSUFBSSxFQUFFLFNBQVM7S0FDaEI7SUFDRCxNQUFNLEVBQUU7UUFDTixTQUFTLEVBQUUsR0FBRztRQUNkLElBQUksRUFBRSxRQUFRO0tBQ2Y7SUFDRCxHQUFHLEVBQUU7UUFDSCxTQUFTLEVBQUUsR0FBRztRQUNkLElBQUksRUFBRSxRQUFRO0tBQ2Y7SUFDRCxNQUFNLEVBQUU7UUFDTixTQUFTLEVBQUUsR0FBRztRQUNkLElBQUksRUFBRSxTQUFTO0tBQ2hCO0lBQ0QsWUFBWSxFQUFFO1FBQ1osU0FBUyxFQUFFLEdBQUc7UUFDZCxJQUFJLEVBQUUsUUFBUTtLQUNmO0lBQ0QsWUFBWSxFQUFFO1FBQ1osSUFBSSxFQUFFLFNBQVM7S0FDaEI7SUFDRCxnQkFBZ0IsRUFBRTtRQUNoQixTQUFTLEVBQUUsR0FBRztRQUNkLElBQUksRUFBRSxTQUFTO0tBQ2hCO0lBQ0QsT0FBTyxFQUFFO1FBQ1AsU0FBUyxFQUFFLEdBQUc7UUFDZCxJQUFJLEVBQUUsU0FBUztLQUNoQjtJ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