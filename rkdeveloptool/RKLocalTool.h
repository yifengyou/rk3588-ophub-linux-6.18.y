#ifndef RKLOCALTOOL_HEADER
#define RKLOCALTOOL_HEADER

#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <stdarg.h>
#include <errno.h>
#include <unistd.h>
#include <fcntl.h>
#include <sys/stat.h>
#include <sys/ioctl.h>
#include <linux/fs.h>
#include <dirent.h>
#include <time.h>
#include <string>
#include <vector>
#include <sstream>
#include <algorithm>
using namespace std;

typedef unsigned char u8;
typedef unsigned char BYTE, *PBYTE;
typedef unsigned short u16;
typedef unsigned short USHORT;
typedef unsigned int u32;
typedef unsigned int UINT;
typedef unsigned int DWORD;
typedef unsigned long long u64;

#define SECTOR_SIZE 512
#define ALIGN(x, a)        __ALIGN_MASK((x), (a) - 1)
#define __ALIGN_MASK(x, mask)    (((x) + (mask)) & ~(mask))
#define CALC_UNIT(a, b)    ((a > 0) ? ((a - 1) / b + 1) : (a))
#define BYTE2SECTOR(x)     (CALC_UNIT(x, SECTOR_SIZE))

#define GPT_HEADER_SIGNATURE 0x5452415020494645ULL
#define GPT_HEADER_REVISION_V1 0x00010000
#define GPT_ENTRY_NUMBERS   128
#define GPT_ENTRY_SIZE      128
#define MSDOS_MBR_SIGNATURE 0xAA55
#define EFI_PMBR_OSTYPE_EFI_GPT 0xEE
#define PART_PROPERTY_BOOTABLE (1 << 2)

#define PARAM_MAGIC 0x4D524150
#define PARAM_OFFSET_SECTOR 0x2000

typedef struct {
    char szItemName[20];
    char szItemValue[256];
} STRUCT_CONFIG_ITEM;
typedef vector<STRUCT_CONFIG_ITEM> CONFIG_ITEM_VECTOR;

typedef struct {
    char szItemName[64];
    UINT uiItemOffset;
    UINT uiItemSize;
} STRUCT_PARAM_ITEM;
typedef vector<STRUCT_PARAM_ITEM> PARAM_ITEM_VECTOR;

typedef union {
    struct {
        unsigned int time_low;
        unsigned short time_mid;
        unsigned short time_hi_and_version;
        unsigned char clock_seq_hi_and_reserved;
        unsigned char clock_seq_low;
        unsigned char node[6];
    } uuid;
    u8 raw[16];
} efi_guid_t;

#define EFI_GUID(a,b,c,d0,d1,d2,d3,d4,d5,d6,d7) \
    ((efi_guid_t) \
    {{ (a) & 0xff, ((a) >> 8) & 0xff, ((a) >> 16) & 0xff, ((a) >> 24) & 0xff, \
        (b) & 0xff, ((b) >> 8) & 0xff, \
        (c) & 0xff, ((c) >> 8) & 0xff, \
        (d0), (d1), (d2), (d3), (d4), (d5), (d6), (d7) }})

#pragma pack(1)
typedef struct _gpt_header {
    u64 signature;
    u32 revision;
    u32 header_size;
    u32 header_crc32;
    u32 reserved1;
    u64 my_lba;
    u64 alternate_lba;
    u64 first_usable_lba;
    u64 last_usable_lba;
    efi_guid_t disk_guid;
    u64 partition_entry_lba;
    u32 num_partition_entries;
    u32 sizeof_partition_entry;
    u32 partition_entry_array_crc32;
} gpt_header;

typedef union _gpt_entry_attributes {
    struct {
        u64 required_to_function:1;
        u64 no_block_io_protocol:1;
        u64 legacy_bios_bootable:1;
        u64 reserved:45;
        u64 type_guid_specific:16;
    } fields;
    unsigned long long raw;
} gpt_entry_attributes;

typedef struct _gpt_entry {
    efi_guid_t partition_type_guid;
    efi_guid_t unique_partition_guid;
    u64 starting_lba;
    u64 ending_lba;
    gpt_entry_attributes attributes;
    u16 partition_name[72 / sizeof(u16)];
} gpt_entry;

typedef struct {
    u8 boot_code[440];
    u32 unique_mbr_signature;
    u16 unknown;
    struct {
        u8 boot_ind;
        u8 head;
        u8 sector;
        u8 cyl;
        u8 sys_ind;
        u8 end_head;
        u8 end_sector;
        u8 end_cyl;
        u32 start_sect;
        u32 nr_sects;
    } partition_record[4];
    u16 signature;
} legacy_mbr;
#pragma pack()

unsigned int crc32_le(unsigned int crc, unsigned char *p, unsigned int len);
void gen_rand_uuid(unsigned char *uuid_bin);
void string_to_uuid(string strUUid, char *uuid);

bool parse_parameter(const char *pParameter, PARAM_ITEM_VECTOR &vecItem, CONFIG_ITEM_VECTOR &vecUuidItem);
bool parse_parameter_file(const char *pParamFile, PARAM_ITEM_VECTOR &vecItem, CONFIG_ITEM_VECTOR &vecUuidItem);
bool parse_config_file(const char *pConfigFile, CONFIG_ITEM_VECTOR &vecItem);
int find_config_item(CONFIG_ITEM_VECTOR &vecItems, const char *pszName);

void create_gpt_buffer(u8 *gpt, PARAM_ITEM_VECTOR &vecParts, CONFIG_ITEM_VECTOR &vecUuid, u64 diskSectors);
void prepare_gpt_backup(u8 *master, u8 *backup);
void update_gpt_disksize(u8 *master, u8 *backup, u32 total_sector);

u64 get_disk_sectors(const char *device);
bool write_sectors(int fd, u64 lba, u32 count, const u8 *buf);
bool read_sectors(int fd, u64 lba, u32 count, u8 *buf);

bool write_gpt_to_disk(const char *device, const char *paramFile);
bool write_parameter_to_disk(const char *device, const char *paramFile);
bool write_image_to_partition(const char *device, UINT partOffset, UINT partSize, const char *imageFile);
bool format_partition(const char *device, UINT partOffset, UINT partSize, const char *partName, int partIndex);
bool write_all(const char *device, const char *paramFile, const char *imageDir, bool doFormat);

void usage_local();

#endif
